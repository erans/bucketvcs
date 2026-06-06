package sshd

import (
	"io"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/shiplog"
)

// countingChannelWriter wraps the SSH session channel's write side and
// counts the bytes written (the fetch/push response bytes flowing back to
// the client). Only Write is intercepted; the engine writes its response
// through Stdout, so this captures the served byte volume.
type countingChannelWriter struct {
	w io.Writer
	n int64
}

func (c *countingChannelWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// countingChannelReader wraps the SSH session channel's read side and counts
// the bytes read (the uploaded packfile flowing in from the client on a
// push). Used by the receive-pack exec path: the push payload is inbound, so
// the read side is the meaningful byte volume (mirroring the HTTPS
// receive-pack handler, which counts the request body).
type countingChannelReader struct {
	r io.Reader
	n int64
}

func (c *countingChannelReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// usageActorName derives the usage-stream actor string from the SSH session
// actor: prefer Name, fall back to UserID, else "anonymous".
func usageActorName(a *auth.Actor) string {
	if a == nil {
		return "anonymous"
	}
	if a.Name != "" {
		return a.Name
	}
	if a.UserID != "" {
		return a.UserID
	}
	return "anonymous"
}

// emitUsage records a fetch/push usage event for an SSH exec. Nil-safe:
// when log shipping is off, s.opts.Usage is nil and this is a no-op.
func (s *Server) emitUsage(kind, tenant, repo string, actor *auth.Actor, bytes int64, start time.Time, serveErr error) {
	if s.opts.Usage == nil {
		return
	}
	status := "ok"
	if serveErr != nil {
		status = "error"
	}
	s.opts.Usage.Usage(shiplog.UsageEvent{
		Kind:       kind,
		Tenant:     tenant,
		Repo:       repo,
		Actor:      usageActorName(actor),
		Transport:  "ssh",
		Bytes:      bytes,
		DurationMS: time.Since(start).Milliseconds(),
		Status:     status,
	})
}
