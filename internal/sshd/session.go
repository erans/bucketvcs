package sshd

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/gitproto/receivepack"
	"github.com/bucketvcs/bucketvcs/internal/gitproto/uploadpack"
)

// handleConn upgrades a raw TCP connection to an SSH server connection,
// dispatches its sessions to handleSession, and closes when done.
func (s *Server) handleConn(ctx context.Context, raw net.Conn) {
	sshConn, chans, reqs, err := ssh.NewServerConn(raw, s.config)
	if err != nil {
		// Auth failures end up here. logAuthAttempt already logged the cause.
		raw.Close()
		return
	}
	defer sshConn.Close()
	go ssh.DiscardRequests(reqs)

	actor, scope, keyID := decodePerms(sshConn.Permissions)
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			newCh.Reject(ssh.UnknownChannelType, "unknown channel type")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			s.opts.Logger.Warn("ssh channel accept failed", "err", err)
			continue
		}
		go s.handleSession(ctx, ch, chReqs, actor, scope, keyID)
	}
}

// decodePerms recovers the typed actor + scope + key id stashed by
// publicKeyCallback into ssh.Permissions.Extensions.
func decodePerms(p *ssh.Permissions) (*auth.Actor, *auth.Scope, string) {
	if p == nil {
		return nil, nil, ""
	}
	isAdmin, _ := strconv.ParseBool(p.Extensions["is_admin"])
	actor := &auth.Actor{
		UserID:  p.Extensions["actor_id"],
		Name:    p.Extensions["actor_name"],
		IsAdmin: isAdmin,
	}
	scope, err := decodeScope(p.Extensions["scope"])
	if err != nil {
		scope = nil
	}
	return actor, scope, p.Extensions["key_id"]
}

// handleSession reads channel requests until "exec" arrives, validates the
// command, applies authorization, and dispatches to the gitproto engine.
// Closes the channel before returning.
func (s *Server) handleSession(ctx context.Context, ch ssh.Channel, reqs <-chan *ssh.Request, actor *auth.Actor, scope *auth.Scope, keyID string) {
	defer ch.Close()

	var protoEnv string
	var execCmd string
	var execReceived bool

	for req := range reqs {
		switch req.Type {
		case "env":
			name, value := parseEnvPayload(req.Payload)
			if name == "GIT_PROTOCOL" {
				protoEnv = value
			}
			req.Reply(true, nil)
		case "exec":
			execCmd = parseExecPayload(req.Payload)
			req.Reply(true, nil)
			execReceived = true
			// Drain the request channel after exec; we'll handle the rest below.
			go discardRemainingRequests(reqs)
			goto run
		case "shell", "pty-req", "subsystem", "x11-req", "auth-agent-req@openssh.com":
			// Never grant a shell or anything else.
			req.Reply(false, nil)
			sendExitStatus(ch, 1)
			return
		default:
			req.Reply(false, nil)
		}
	}
	return // channel closed without exec

run:
	if !execReceived || strings.TrimSpace(execCmd) == "" {
		sendStderrLine(ch, "bucketvcs: empty exec command")
		sendExitStatus(ch, 128)
		return
	}
	cmd, err := ParseExecCommand(execCmd)
	if err != nil {
		sendStderrLine(ch, "bucketvcs: "+err.Error())
		sendExitStatus(ch, 128)
		return
	}

	flags, err := s.opts.Store.GetRepoFlags(ctx, cmd.Tenant, cmd.Repo)
	if err != nil {
		if errors.Is(err, auth.ErrNoSuchRepo) {
			sendStderrLine(ch, "bucketvcs: repository not found")
			sendExitStatus(ch, 128)
			return
		}
		sendStderrLine(ch, "bucketvcs: internal error")
		sendExitStatus(ch, 1)
		return
	}

	var perm auth.Perm
	if scope != nil {
		if scope.Tenant != cmd.Tenant || scope.Repo != cmd.Repo {
			sendStderrLine(ch, "bucketvcs: key not authorized for this repository")
			sendExitStatus(ch, 128)
			return
		}
		perm = scope.Perm
	} else {
		p, err := s.opts.Store.LookupRepoPerm(ctx, actor, cmd.Tenant, cmd.Repo)
		if err != nil {
			sendStderrLine(ch, "bucketvcs: internal error")
			sendExitStatus(ch, 1)
			return
		}
		perm = p
	}

	ok, _ := auth.Decide(actor, perm, cmd.Op.RequiredAction(), flags)
	if !ok {
		sendStderrLine(ch, "bucketvcs: insufficient permissions")
		sendExitStatus(ch, 128)
		return
	}

	pv := 0
	if protoEnv == "version=2" {
		pv = 2
	}

	var serveErr error
	switch cmd.Op {
	case OpUpload:
		req := &uploadpack.EngineRequest{
			Ctx:             ctx,
			Tenant:          cmd.Tenant,
			Repo:            cmd.Repo,
			Actor:           actor,
			Stdin:           ch,
			Stdout:          ch,
			Stderr:          ch.Stderr(),
			ProtocolVersion: pv,
			Store:           s.opts.BVStore,
			Mirror:          s.opts.Mirror,
			AgentVersion:    s.opts.AgentVersion,
		}
		serveErr = uploadpack.Serve(req)
	case OpReceive:
		req := &receivepack.EngineRequest{
			Ctx:             ctx,
			Tenant:          cmd.Tenant,
			Repo:            cmd.Repo,
			Actor:           actor,
			Stdin:           ch,
			Stdout:          ch,
			Stderr:          ch.Stderr(),
			ProtocolVersion: pv,
			Store:           s.opts.BVStore,
			Mirror:          s.opts.Mirror,
			AgentVersion:    s.opts.AgentVersion,
		}
		serveErr = receivepack.Serve(req)
	}

	// Best-effort touch of last_used_at; don't block.
	if keyID != "" {
		go func() {
			_ = s.opts.Store.TouchSSHKeyUsage(context.Background(), keyID)
		}()
	}

	if serveErr != nil {
		s.opts.Logger.Warn("ssh engine error",
			"tenant", cmd.Tenant, "repo", cmd.Repo, "op", cmd.Op, "err", serveErr)
		sendExitStatus(ch, 1)
		return
	}
	sendExitStatus(ch, 0)
}

// discardRemainingRequests drains a request channel after exec was accepted.
// Future env/window-change requests are replied with false but we don't care.
func discardRemainingRequests(reqs <-chan *ssh.Request) {
	for req := range reqs {
		req.Reply(false, nil)
	}
}

// parseExecPayload extracts the command string from an SSH exec request.
// The wire format (RFC 4254 §6.5) is: uint32 length || string command.
func parseExecPayload(payload []byte) string {
	if len(payload) < 4 {
		return ""
	}
	n := binary.BigEndian.Uint32(payload[:4])
	if int(n)+4 > len(payload) {
		return ""
	}
	return string(payload[4 : 4+n])
}

// parseEnvPayload extracts (name, value) from an SSH env request.
// Wire format: uint32 nameLen || name || uint32 valueLen || value.
func parseEnvPayload(payload []byte) (string, string) {
	if len(payload) < 4 {
		return "", ""
	}
	nl := binary.BigEndian.Uint32(payload[:4])
	if int(nl)+4 > len(payload) {
		return "", ""
	}
	name := string(payload[4 : 4+nl])
	rest := payload[4+nl:]
	if len(rest) < 4 {
		return name, ""
	}
	vl := binary.BigEndian.Uint32(rest[:4])
	if int(vl)+4 > len(rest) {
		return name, ""
	}
	return name, string(rest[4 : 4+vl])
}

// sendStderrLine writes one error line to the channel's stderr.
func sendStderrLine(ch ssh.Channel, msg string) {
	fmt.Fprintln(ch.Stderr(), msg)
}

// sendExitStatus sends the exit-status request that terminates a channel.
func sendExitStatus(ch ssh.Channel, code uint32) {
	msg := struct{ Status uint32 }{Status: code}
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, msg) //nolint:errcheck
	_, _ = ch.SendRequest("exit-status", false, buf.Bytes())
}
