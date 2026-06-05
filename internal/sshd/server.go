package sshd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/ratelimit"
	"github.com/bucketvcs/bucketvcs/internal/hooks"
	"github.com/bucketvcs/bucketvcs/internal/lfs"
	"github.com/bucketvcs/bucketvcs/internal/mirror"
	"github.com/bucketvcs/bucketvcs/internal/policy"
	"github.com/bucketvcs/bucketvcs/internal/replica"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

// Options configures the SSH listener and the underlying engine seam.
type Options struct {
	// Addr is the TCP listen address, e.g. "127.0.0.1:2222" or ":2222".
	Addr string
	// HostKeyPath is the location of the OpenSSH-format private key file.
	// If the file is absent, NewServer generates an ed25519 key and
	// persists it (mode 0600).
	HostKeyPath string
	// Grace bounds Close()'s wait for in-flight sessions before forcing
	// closure. Zero means force-close immediately.
	Grace time.Duration

	// AgentVersion is the gateway's advertised agent version, plumbed into
	// gitproto EngineRequest.
	AgentVersion string

	// Auth + storage seams.
	Store   auth.Store
	BVStore storage.ObjectStore
	Mirror  *mirror.Manager
	Logger  *slog.Logger

	// BundleURIEnabled, BundleWarmCommits, BundleWarmAge, and
	// BundleURIBuildURL are forwarded directly into uploadpack.EngineRequest.
	// See gateway.Options for field semantics.
	BundleURIEnabled  bool
	BundleWarmCommits int
	BundleWarmAge     time.Duration
	// BundleURIBuildURL mints the URL advertised in command=bundle-uri
	// responses. nil disables the feature (empty response).
	BundleURIBuildURL func(ctx context.Context, tenant, repo, hash, storageKey, expectedHash string) (string, error)

	// PackURIEnabled and PackURIBuildURL gate the packfile-uris capability
	// for SSH; semantics mirror BundleURI*. PackURIBuildURL is required
	// when PackURIEnabled is true.
	PackURIEnabled  bool
	PackURIBuildURL func(ctx context.Context, tenant, repo, hash, storageKey, expectedHash string) (string, error)

	// LFSTokenIssuer mints short-TTL HTTP bearers for the
	// git-lfs-authenticate command. When nil, the command is rejected
	// with "lfs not enabled".
	LFSTokenIssuer lfs.TokenIssuer

	// LFSBaseURL is the external base URL of the HTTPS gateway, used
	// to construct the Href in the LFS SSH-authenticate response
	// (e.g. "https://gw.example"). Required when LFSTokenIssuer is set.
	LFSBaseURL string

	// LFSSSHTokenTTL is the lifetime of bearers issued via SSH
	// git-lfs-authenticate. Zero falls back to 15 minutes (applied
	// in NewServer); negative values are rejected at NewServer time.
	LFSSSHTokenTTL time.Duration

	// Policy enables M14 protected-refs enforcement in receive-pack
	// step 8b on the SSH transport. nil = pre-M14 behavior (all ref
	// updates accepted). Mirrors gateway.Options.Policy.
	Policy *policy.Service

	// Webhooks enables M15 webhook emission for SSH receive-pack.
	// Mirrors gateway.Options.Webhooks. nil disables all enqueues.
	Webhooks *webhooks.Service

	// Hooks is OPTIONAL. When non-nil, EngineRequest.Hooks is populated
	// for receive-pack and pre-receive/post-receive subprocess execution
	// runs on the SSH transport. Mirrors gateway.Options.Hooks. nil means
	// hooks disabled (M20).
	Hooks *hooks.Service

	// Limiter throttles repeated key-rejections from a single IP. Nil
	// disables rate limiting entirely (Check / MarkFailure / MarkSuccess
	// are all no-ops on a nil receiver). Mirrors gateway.Options.Limiter.
	// See spec §30.5 / M18.
	Limiter *ratelimit.Limiter

	// Replica marks this SSH gateway as a read-only regional replica:
	// git-receive-pack sessions are refused with a pointer to
	// WriteRegionURL; git-upload-pack consults Replica.Gate. Mirrors the
	// HTTP gateway's Options.Replica semantics. See internal/replica.
	Replica *replica.GatewayConfig

	// Resolver, when non-nil, enables BYOB (Bring Your Own Bucket) mode:
	// each git-upload-pack and git-receive-pack session calls
	// Resolver.Resolve(ctx, tenant) to obtain the per-tenant ObjectStore.
	// When nil, s.opts.BVStore is used directly (default behavior).
	Resolver byobResolver
}

// Server is the bucketvcs SSH listener. Construct via NewServer.
type Server struct {
	opts     Options
	config   *ssh.ServerConfig
	listener net.Listener

	mu       sync.Mutex
	closed   bool
	sessions sync.WaitGroup
}

// NewServer loads or generates the host key, builds the ssh.ServerConfig,
// and returns a ready-to-Listen Server. The actual TCP listen happens
// in Listen() (Task 21).
func NewServer(opts Options) (*Server, error) {
	if opts.Logger == nil {
		return nil, errors.New("sshd: Options.Logger is required")
	}
	if opts.Store == nil {
		return nil, errors.New("sshd: Options.Store is required")
	}
	if opts.HostKeyPath == "" {
		return nil, errors.New("sshd: Options.HostKeyPath is required")
	}
	if opts.Addr == "" {
		return nil, errors.New("sshd: Options.Addr is required")
	}

	// Apply BundleURI defaults so an SSH operator who enables BundleURI
	// without explicit warm-window values gets the same behavior as the
	// HTTP gateway (gateway.NewServer applies identical defaults). Without
	// these, BundleWarmCommits=0 causes IsAncestor(_, _, 0) to return false
	// for every non-current bundle and the cap silently becomes a no-op.
	if opts.BundleURIEnabled {
		if opts.BundleURIBuildURL == nil {
			return nil, errors.New("sshd: Options.BundleURIBuildURL is required when BundleURIEnabled is true")
		}
		if opts.BundleWarmCommits == 0 {
			opts.BundleWarmCommits = 5000
		}
		if opts.BundleWarmAge == 0 {
			opts.BundleWarmAge = 24 * time.Hour
		}
	}
	if opts.PackURIEnabled && opts.PackURIBuildURL == nil {
		return nil, errors.New("sshd: Options.PackURIBuildURL is required when PackURIEnabled is true")
	}
	// LFS SSH-authenticate: when the operator wires an issuer they must
	// also supply the external base URL; the TTL has a sane default but
	// negative values are a misconfiguration. Symmetric check: setting
	// any LFS field without LFSTokenIssuer is a configuration footgun
	// (the SSH path will silently land on "lfs not enabled"), reject.
	if opts.LFSTokenIssuer != nil {
		if opts.LFSBaseURL == "" {
			return nil, errors.New("sshd: Options.LFSBaseURL is required when LFSTokenIssuer is set")
		}
		if opts.LFSSSHTokenTTL < 0 {
			return nil, errors.New("sshd: Options.LFSSSHTokenTTL must be >= 0 (0 means use the default)")
		}
		if opts.LFSSSHTokenTTL == 0 {
			opts.LFSSSHTokenTTL = 15 * time.Minute
		}
	} else if opts.LFSBaseURL != "" || opts.LFSSSHTokenTTL != 0 {
		return nil, errors.New("sshd: Options.LFSBaseURL/LFSSSHTokenTTL set without LFSTokenIssuer")
	}

	signer, err := LoadOrGenerateHostKey(opts.HostKeyPath, opts.Logger)
	if err != nil {
		return nil, err
	}

	s := &Server{opts: opts}
	s.config = &ssh.ServerConfig{
		MaxAuthTries:      6,
		PublicKeyCallback: s.publicKeyCallback,
		AuthLogCallback:   s.logAuthAttempt,
	}
	s.config.AddHostKey(signer)
	return s, nil
}

// publicKeyCallback authenticates a presented public key against the auth
// Store. On success, the actor + scope + key id are stashed in
// ssh.Permissions.Extensions for the session handler.
//
// M18: this callback is the SSH-side rate-limit point. The Limiter is
// consulted BEFORE the store is touched, so a rate-limited client never
// reaches VerifyCredential. The username is unknown pre-resolution (SSH
// always presents user="git"), so Check uses ip + empty user. On
// credential-state errors we MarkFailure(ip, "") to increment the IP
// bucket; on success we MarkSuccess(ip, resolvedUser) to reset both the
// IP bucket and (best-effort) the resolved user's bucket. Internal-state
// errors (DB unreachable, etc.) are NOT counted as failures — they
// would otherwise let a flaky backend lock out legitimate clients.
func (s *Server) publicKeyCallback(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	if meta.User() != "git" {
		return nil, errors.New("only the 'git' user is supported")
	}

	// x/crypto/ssh has no per-callback context; pass background. The Store
	// implementations honor ctx for cancellation but we have none here.
	ctx := context.Background()

	// M18 — rate-limit gate before any store call. The user is unknown at
	// this point (SSH client always sends user="git"), so we check only
	// the IP bucket. SSH has no Retry-After equivalent in the protocol;
	// we just close the connection with an error.
	ip := sshRemoteIP(meta)
	if allowed, retryAfter, _ := s.opts.Limiter.CheckDetailed(ip, ""); !allowed {
		retrySec := int(retryAfter.Seconds())
		if retrySec < 1 {
			retrySec = 1
		}
		auth.EmitRateLimitHit(ctx, s.opts.Logger, ip, "", "ip", retrySec, "ssh")
		ratelimit.EmitRateLimitMetric(ctx, s.opts.Logger, "limited_ip")
		return nil, errors.New("rate limited")
	}

	fp := SHA256Fingerprint(key)
	actor, keyID, scope, err := s.opts.Store.VerifyCredential(
		ctx,
		auth.SSHKeyFingerprint{Fingerprint: fp},
	)
	if err != nil {
		// Only credential-state errors count as failures. Backend / internal
		// errors (DB unreachable, etc.) must not bump the bucket — otherwise
		// a flaky store could lock out legitimate clients.
		if auth.IsCredentialError(err) {
			s.opts.Limiter.MarkFailure(ip, "")
			ratelimit.EmitRateLimitMetric(ctx, s.opts.Logger, "failure_counted")
		}
		return nil, err
	}

	// Successful key verification resets the rate-limit bucket. Use the
	// resolved actor name so the per-user bucket is also reset; the IP
	// bucket is reset either way (MarkSuccess(ip, "") still clears it).
	user := ""
	if actor != nil {
		user = actor.Name
	}
	s.opts.Limiter.MarkSuccess(ip, user)
	ratelimit.EmitRateLimitMetric(ctx, s.opts.Logger, "success_reset")

	return &ssh.Permissions{Extensions: map[string]string{
		"actor_id":   actor.UserID,
		"actor_name": actor.Name,
		"is_admin":   strconv.FormatBool(actor.IsAdmin),
		"scope":      encodeScope(scope),
		"key_id":     keyID,
	}}, nil
}

// sshRemoteIP returns the host portion of an SSH connection's RemoteAddr,
// stripping the port. Falls back to the full RemoteAddr string when the
// host:port split fails (e.g., unusual transports in tests). Returns ""
// when meta.RemoteAddr() is nil — golang.org/x/crypto/ssh does not recover
// panics from auth callbacks, so the nil guard prevents a process-crashing
// dereference if the transport ever yields a nil addr.
func sshRemoteIP(meta ssh.ConnMetadata) string {
	a := meta.RemoteAddr()
	if a == nil {
		return ""
	}
	addr := a.String()
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return addr
	}
	return host
}

// logAuthAttempt records every SSH auth attempt (success or failure).
func (s *Server) logAuthAttempt(meta ssh.ConnMetadata, method string, err error) {
	fields := []any{
		"remote", meta.RemoteAddr().String(),
		"user", meta.User(),
		"method", method,
	}
	if err != nil {
		fields = append(fields, "result", "fail", "err", err.Error())
	} else {
		fields = append(fields, "result", "ok")
	}
	s.opts.Logger.Info("ssh auth attempt", fields...)
}

// encodeScope serializes a *Scope for ssh.Permissions.Extensions, which
// only carries strings. Empty for nil; "<tenant>/<repo>:<read|write>"
// otherwise.
func encodeScope(scope *auth.Scope) string {
	if scope == nil {
		return ""
	}
	return scope.Tenant + "/" + scope.Repo + ":" + auth.PermToText(scope.Perm)
}

// Listen opens the TCP listener on opts.Addr. Call before Serve.
func (s *Server) Listen() error {
	l, err := net.Listen("tcp", s.opts.Addr)
	if err != nil {
		return fmt.Errorf("ssh listen %s: %w", s.opts.Addr, err)
	}
	s.listener = l
	s.opts.Logger.Info("ssh listening", "addr", l.Addr().String())
	return nil
}

// Addr returns the actual listen address (useful when Addr was ":0" for tests).
func (s *Server) Addr() net.Addr {
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

// Serve accepts connections until ctx is canceled or Close() is called.
// Blocks. Returns nil on graceful shutdown, or the accept error.
func (s *Server) Serve(ctx context.Context) error {
	if s.listener == nil {
		return errors.New("sshd: Listen() not called")
	}
	go func() {
		<-ctx.Done()
		s.listener.Close()
	}()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed || ctx.Err() != nil {
				return nil
			}
			return err
		}
		s.sessions.Add(1)
		go func() {
			defer s.sessions.Done()
			s.handleConn(ctx, conn)
		}()
	}
}

// Close stops accepting new connections and waits up to opts.Grace for
// in-flight sessions to drain. After Grace, in-flight ssh.Channels are
// closed by killing the listener (sshConn.Close in handleConn).
func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	if s.listener != nil {
		s.listener.Close()
	}
	done := make(chan struct{})
	go func() {
		s.sessions.Wait()
		close(done)
	}()
	grace := s.opts.Grace
	if grace == 0 {
		return nil
	}
	select {
	case <-done:
	case <-time.After(grace):
		s.opts.Logger.Warn("ssh grace exceeded; in-flight sessions left to terminate")
	}
	return nil
}

// decodeScope is the inverse. Empty string -> nil.
func decodeScope(s string) (*auth.Scope, error) {
	if s == "" {
		return nil, nil
	}
	// <tenant>/<repo>:<read|write>
	colon := -1
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			colon = i
			break
		}
	}
	if colon < 0 {
		return nil, fmt.Errorf("decodeScope: missing ':' in %q", s)
	}
	repoPart := s[:colon]
	permPart := s[colon+1:]
	slash := -1
	for i := 0; i < len(repoPart); i++ {
		if repoPart[i] == '/' {
			slash = i
			break
		}
	}
	if slash < 0 {
		return nil, fmt.Errorf("decodeScope: missing '/' in %q", s)
	}
	perm, err := auth.PermFromText(permPart)
	if err != nil {
		return nil, err
	}
	return &auth.Scope{
		Tenant: repoPart[:slash],
		Repo:   repoPart[slash+1:],
		Perm:   perm,
	}, nil
}
