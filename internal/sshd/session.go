package sshd

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/gitproto/receivepack"
	"github.com/bucketvcs/bucketvcs/internal/gitproto/uploadpack"
	"github.com/bucketvcs/bucketvcs/internal/lfs"
	"github.com/bucketvcs/bucketvcs/internal/replica"
	"github.com/bucketvcs/bucketvcs/internal/shiplog"
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

	// OpLFSAuthenticate owns its own auth.Decide inside the handler
	// because it refines the action by LFSOp (upload=Write, download=Read).
	// Other ops use the static RequiredAction here.
	if cmd.Op != OpLFSAuthenticate {
		ok, _ := auth.Decide(actor, perm, cmd.Op.RequiredAction(), flags)
		if !ok {
			sendStderrLine(ch, "bucketvcs: insufficient permissions")
			sendExitStatus(ch, 128)
			return
		}
	}

	pv := 0
	if protoEnv == "version=2" {
		pv = 2
	}

	var serveErr error
	switch cmd.Op {
	case OpUpload:
		// Read-only replica freshness gate: a bounded-stale replica past
		// its lag budget refuses ref advertisement (mirrors the HTTP
		// gateway's 503). The gate error string carries the diagnostic;
		// we surface it on stderr and exit non-zero before advertising.
		// Same terminal-reject idiom as the OpReceive refusal above.
		if s.opts.Replica != nil && s.opts.Replica.Gate != nil {
			if err := s.opts.Replica.Gate.CheckAdvertise(ctx, cmd.Tenant, cmd.Repo); err != nil {
				sendStderrLine(ch, err.Error())
				sendExitStatus(ch, 128)
				return
			}
		}
		store, err := s.resolveStore(ctx, cmd.Tenant)
		if err != nil {
			sendStderrLine(ch, "storage error: "+err.Error())
			sendExitStatus(ch, 1)
			return
		}
		// Usage metering: count fetch response bytes on the channel write
		// side. start is taken here, the point we commit to serving.
		uploadCW := &countingChannelWriter{w: ch}
		uploadStart := time.Now()
		req := &uploadpack.EngineRequest{
			Ctx:               ctx,
			Tenant:            cmd.Tenant,
			Repo:              cmd.Repo,
			Actor:             actor,
			Stdin:             ch,
			Stdout:            uploadCW,
			Stderr:            ch.Stderr(),
			ProtocolVersion:   pv,
			SSH:               true,
			Store:             store,
			Mirror:            s.opts.Mirror,
			AgentVersion:      s.opts.AgentVersion,
			BundleURIEnabled:  s.opts.BundleURIEnabled,
			BundleURIBuildURL: s.opts.BundleURIBuildURL,
			BundleWarmCommits: s.opts.BundleWarmCommits,
			BundleWarmAge:     s.opts.BundleWarmAge,
			PackURIEnabled:    s.opts.PackURIEnabled,
			PackURIBuildURL:   s.opts.PackURIBuildURL,
			Logger:            s.opts.Logger,
		}
		// Advertise once, then handle multiple commands until EOF.
		// Over SSH the channel stays open; git sends multiple commands
		// (e.g. ls-refs then fetch) sequentially before closing.
		if err := uploadpack.Advertise(req); err != nil {
			serveErr = err
			break
		}
		for {
			err := uploadpack.Service(req)
			if err == uploadpack.ErrEOF {
				break
			}
			if err != nil {
				serveErr = err
				break
			}
		}
		s.emitUsage(shiplog.KindFetch, cmd.Tenant, cmd.Repo, actor, uploadCW.n, uploadStart, serveErr)
	case OpReceive:
		// Read-only replica: refuse pushes with a pointer to the write
		// region, matching the HTTP gateway's 403 refusal. We emit the
		// refusal on stderr and exit non-zero before building the engine
		// request, following this function's terminal-reject idiom
		// (sendStderrLine + sendExitStatus + return, like the perm-denied
		// and parse-error paths above). The trailing best-effort
		// TouchSSHKeyUsage is intentionally skipped — a refused write is
		// not a successful key use.
		if s.opts.Replica != nil {
			sendStderrLine(ch, replica.RefusalMessage(s.opts.Replica.WriteRegionURL))
			sendExitStatus(ch, 128)
			return
		}
		store, err := s.resolveStore(ctx, cmd.Tenant)
		if err != nil {
			sendStderrLine(ch, "storage error: "+err.Error())
			sendExitStatus(ch, 1)
			return
		}
		// Usage metering: count the uploaded packfile bytes on the read
		// side (the push payload is inbound). start is taken here.
		receiveCR := &countingChannelReader{r: ch}
		receiveStart := time.Now()
		req := &receivepack.EngineRequest{
			Ctx:             ctx,
			Tenant:          cmd.Tenant,
			Repo:            cmd.Repo,
			Actor:           actor,
			Stdin:           receiveCR,
			Stdout:          ch,
			Stderr:          ch.Stderr(),
			ProtocolVersion: pv,
			Store:           store,
			Mirror:          s.opts.Mirror,
			AgentVersion:    s.opts.AgentVersion,
			Policy:          s.opts.Policy,
			Webhooks:        s.opts.Webhooks,
			BuildTriggers:   s.opts.BuildTriggers,
			Hooks:           s.opts.Hooks,
			Logger:          s.opts.Logger,
		}
		serveErr = receivepack.Serve(req)
		// The flush-only probe carries no pack and is not a push attempt,
		// so it is excluded from usage metering (mirrors HTTPS receive_pack.go).
		if !errors.Is(serveErr, receivepack.ErrFlushOnlyProbe) {
			s.emitUsage(shiplog.KindPush, cmd.Tenant, cmd.Repo, actor, receiveCR.n, receiveStart, serveErr)
		}
	case OpLFSAuthenticate:
		// handleLFSAuthenticate owns its own exit code (0 on success,
		// 1 on disabled/error, 128 on forbidden/anon). Returning here
		// avoids the trailing sendExitStatus(ch, 0) that would otherwise
		// overwrite the bespoke status.
		err := s.handleLFSAuthenticate(ctx, cmd, actor, ch, perm, flags)
		if keyID != "" {
			go func() {
				_ = s.opts.Store.TouchSSHKeyUsage(context.Background(), keyID)
			}()
		}
		if err != nil {
			s.opts.Logger.Warn("ssh engine error",
				"tenant", cmd.Tenant, "repo", cmd.Repo, "op", cmd.Op, "err", err)
		}
		return
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

// handleLFSAuthenticate dispatches the git-lfs-authenticate exec command:
// mints a short-TTL HTTP bearer via lfs.IssueSSHToken and writes the JSON
// response (href, header, expires_at) to stdout. Errors and policy denials
// go to stderr with bespoke exit codes (1 disabled/error, 128 forbidden/anon).
// Each terminal path emits a paired lfs_ssh_authenticate_total metric and
// lfs.ssh_authenticate audit event via lfs.EmitSSHAuthenticateMetric /
// lfs.EmitLFSSSHAuthenticate.
func (s *Server) handleLFSAuthenticate(ctx context.Context, cmd *ExecCommand, actor *auth.Actor, ch ssh.Channel, perm auth.Perm, flags auth.RepoFlags) error {
	logger := s.opts.Logger
	repoFQN := cmd.Tenant + "/" + cmd.Repo
	user := ""
	if actor != nil {
		user = actor.Name
	}

	// Authorize FIRST so a fully unauthorized actor sees "forbidden"
	// regardless of LFS toggle state — avoids leaking the LFS-enabled
	// bit through error messages. auth.Decide is cheap (in-memory perm
	// lookup was already resolved by handleSession before dispatch).
	required := auth.ActionWrite
	if cmd.LFSOp == "download" {
		required = auth.ActionRead
	}
	ok, _ := auth.Decide(actor, perm, required, flags)
	if !ok {
		lfs.EmitSSHAuthenticateMetric(ctx, logger, cmd.LFSOp, "forbidden")
		lfs.EmitLFSSSHAuthenticate(ctx, logger, repoFQN, user, cmd.LFSOp, 0, "forbidden")
		sendStderrLine(ch, "bucketvcs: insufficient permissions")
		sendExitStatus(ch, 128)
		return nil
	}

	if s.opts.LFSTokenIssuer == nil || s.opts.LFSBaseURL == "" {
		lfs.EmitSSHAuthenticateMetric(ctx, logger, cmd.LFSOp, "disabled")
		lfs.EmitLFSSSHAuthenticate(ctx, logger, repoFQN, user, cmd.LFSOp, 0, "disabled")
		sendStderrLine(ch, "bucketvcs: lfs not enabled on this gateway")
		sendExitStatus(ch, 1)
		return nil
	}

	// Anonymous actors and deploy-key actors cannot mint tokens
	// (deploy-key UserIDs aren't real users.id FK values).
	if actor == nil || actor.UserID == "" || strings.HasPrefix(actor.UserID, "deploy:") {
		lfs.EmitSSHAuthenticateMetric(ctx, logger, cmd.LFSOp, "anon")
		lfs.EmitLFSSSHAuthenticate(ctx, logger, repoFQN, user, cmd.LFSOp, 0, "anon")
		sendStderrLine(ch, "bucketvcs: anonymous and deploy keys cannot mint LFS bearers")
		sendExitStatus(ch, 128)
		return nil
	}

	// LFSSSHTokenTTL was defaulted + validated in NewServer.
	ttl := s.opts.LFSSSHTokenTTL
	resp, err := lfs.IssueSSHToken(ctx, s.opts.LFSTokenIssuer, actor.UserID, actor.Name, cmd.Tenant, cmd.Repo, cmd.LFSOp, s.opts.LFSBaseURL, ttl)
	if err != nil {
		logger.Warn("lfs IssueSSHToken failed", "err", err)
		lfs.EmitSSHAuthenticateMetric(ctx, logger, cmd.LFSOp, "error")
		lfs.EmitLFSSSHAuthenticate(ctx, logger, repoFQN, user, cmd.LFSOp, int64(ttl/time.Second), "error")
		sendStderrLine(ch, "bucketvcs: failed to issue lfs token")
		sendExitStatus(ch, 1)
		return nil
	}

	body, err := json.Marshal(resp)
	if err != nil {
		lfs.EmitSSHAuthenticateMetric(ctx, logger, cmd.LFSOp, "error")
		lfs.EmitLFSSSHAuthenticate(ctx, logger, repoFQN, user, cmd.LFSOp, int64(ttl/time.Second), "error")
		sendStderrLine(ch, "bucketvcs: marshal failed")
		sendExitStatus(ch, 1)
		return nil
	}
	if _, err := ch.Write(append(body, '\n')); err != nil {
		// Token was minted and persisted but the client never saw it;
		// it will expire on its TTL (default 15m), which is preferable
		// to coupling lfs.TokenIssuer with a Delete API for this rare
		// path. Emit an explicit terminal outcome so the operator can
		// correlate the dangling row with the disconnect.
		lfs.EmitSSHAuthenticateMetric(ctx, logger, cmd.LFSOp, "client_disconnected")
		lfs.EmitLFSSSHAuthenticate(ctx, logger, repoFQN, user, cmd.LFSOp, int64(ttl/time.Second), "client_disconnected")
		// Best-effort exit status to match the marshal-failure branch
		// above; channel is broken so this may fail silently.
		sendExitStatus(ch, 1)
		return err
	}
	lfs.EmitSSHAuthenticateMetric(ctx, logger, cmd.LFSOp, "ok")
	lfs.EmitLFSSSHAuthenticate(ctx, logger, repoFQN, user, cmd.LFSOp, int64(ttl/time.Second), "ok")
	sendExitStatus(ch, 0)
	return nil
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
