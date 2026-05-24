package sshd

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/ratelimit"
)

func newTestServerOpts(t *testing.T, store auth.Store) Options {
	t.Helper()
	dir := t.TempDir()
	return Options{
		Addr:        "127.0.0.1:0",
		HostKeyPath: filepath.Join(dir, "host_key"),
		Grace:       0,
		Store:       store,
		Logger:      slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
}

// fakeStore mocks auth.Store for SSH callback tests.
type fakeStore struct {
	verify func(c auth.Credential) (*auth.Actor, string, *auth.Scope, error)
}

func (f *fakeStore) VerifyCredential(ctx context.Context, c auth.Credential) (*auth.Actor, string, *auth.Scope, error) {
	return f.verify(c)
}
func (f *fakeStore) LookupRepoPerm(ctx context.Context, actor *auth.Actor, tenant, repo string) (auth.Perm, error) {
	return auth.PermNone, nil
}
func (f *fakeStore) GetRepoFlags(ctx context.Context, tenant, repo string) (auth.RepoFlags, error) {
	return auth.RepoFlags{}, nil
}
func (f *fakeStore) TouchTokenUsage(ctx context.Context, tokenID string) error { return nil }
func (f *fakeStore) AddSSHKey(ctx context.Context, k auth.SSHKey) error        { return nil }
func (f *fakeStore) ListSSHKeysForUser(ctx context.Context, userID string) ([]auth.SSHKey, error) {
	return nil, nil
}
func (f *fakeStore) ListSSHKeysForRepo(ctx context.Context, tenant, repo string) ([]auth.SSHKey, error) {
	return nil, nil
}
func (f *fakeStore) RevokeSSHKey(ctx context.Context, keyIDOrPrefix string) error { return nil }
func (f *fakeStore) TouchSSHKeyUsage(ctx context.Context, keyID string) error     { return nil }
func (f *fakeStore) Close() error                                                 { return nil }
func (f *fakeStore) GetUserByName(ctx context.Context, name string) (*auth.User, error) {
	return nil, auth.ErrNoSuchUser
}

func TestNewServer_RequiresFields(t *testing.T) {
	base := newTestServerOpts(t, &fakeStore{})
	cases := []struct {
		name string
		mut  func(*Options)
	}{
		{"missing addr", func(o *Options) { o.Addr = "" }},
		{"missing logger", func(o *Options) { o.Logger = nil }},
		{"missing store", func(o *Options) { o.Store = nil }},
		{"missing hostkey", func(o *Options) { o.HostKeyPath = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := base
			tc.mut(&opts)
			_, err := NewServer(opts)
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestPublicKeyCallback_RejectsNonGitUser(t *testing.T) {
	store := &fakeStore{verify: func(c auth.Credential) (*auth.Actor, string, *auth.Scope, error) {
		return &auth.Actor{UserID: "u1"}, "k1", nil, nil
	}}
	s, err := NewServer(newTestServerOpts(t, store))
	if err != nil {
		t.Fatal(err)
	}

	pub := mustReadAuthorizedKey(t, "testdata/ed25519.pub")
	meta := stubConnMetadata{user: "alice"}
	_, err = s.publicKeyCallback(meta, pub)
	if err == nil {
		t.Fatal("expected non-git rejection")
	}
}

func TestPublicKeyCallback_UserKey_ScopeNil(t *testing.T) {
	store := &fakeStore{verify: func(c auth.Credential) (*auth.Actor, string, *auth.Scope, error) {
		return &auth.Actor{UserID: "u1", Name: "alice"}, "kabc", nil, nil
	}}
	s, err := NewServer(newTestServerOpts(t, store))
	if err != nil {
		t.Fatal(err)
	}
	pub := mustReadAuthorizedKey(t, "testdata/ed25519.pub")
	meta := stubConnMetadata{user: "git"}
	perms, err := s.publicKeyCallback(meta, pub)
	if err != nil {
		t.Fatal(err)
	}
	if perms.Extensions["actor_id"] != "u1" {
		t.Fatalf("actor_id = %q", perms.Extensions["actor_id"])
	}
	if perms.Extensions["scope"] != "" {
		t.Fatalf("scope = %q, want empty", perms.Extensions["scope"])
	}
	if perms.Extensions["key_id"] != "kabc" {
		t.Fatalf("key_id = %q", perms.Extensions["key_id"])
	}
}

func TestPublicKeyCallback_DeployKey_ScopeEncoded(t *testing.T) {
	store := &fakeStore{verify: func(c auth.Credential) (*auth.Actor, string, *auth.Scope, error) {
		return &auth.Actor{UserID: "deploy:k1", Name: "deploy-key:ci"},
			"k1",
			&auth.Scope{Tenant: "acme", Repo: "web", Perm: auth.PermWrite},
			nil
	}}
	s, err := NewServer(newTestServerOpts(t, store))
	if err != nil {
		t.Fatal(err)
	}
	pub := mustReadAuthorizedKey(t, "testdata/ed25519.pub")
	meta := stubConnMetadata{user: "git"}
	perms, err := s.publicKeyCallback(meta, pub)
	if err != nil {
		t.Fatal(err)
	}
	want := "acme/web:write"
	if perms.Extensions["scope"] != want {
		t.Fatalf("scope = %q, want %q", perms.Extensions["scope"], want)
	}
}

func TestPublicKeyCallback_VerifyError(t *testing.T) {
	store := &fakeStore{verify: func(c auth.Credential) (*auth.Actor, string, *auth.Scope, error) {
		return nil, "", nil, auth.ErrInvalidCredential
	}}
	s, err := NewServer(newTestServerOpts(t, store))
	if err != nil {
		t.Fatal(err)
	}
	pub := mustReadAuthorizedKey(t, "testdata/ed25519.pub")
	meta := stubConnMetadata{user: "git"}
	_, err = s.publicKeyCallback(meta, pub)
	if !errors.Is(err, auth.ErrInvalidCredential) {
		t.Fatalf("got %v, want ErrInvalidCredential", err)
	}
}

func TestEncodeDecodeScope_RoundTrip(t *testing.T) {
	cases := []*auth.Scope{
		nil,
		{Tenant: "acme", Repo: "web", Perm: auth.PermRead},
		{Tenant: "acme", Repo: "web", Perm: auth.PermWrite},
	}
	for _, want := range cases {
		s := encodeScope(want)
		got, err := decodeScope(s)
		if err != nil {
			t.Fatalf("decodeScope: %v", err)
		}
		if (got == nil) != (want == nil) {
			t.Fatalf("nilness mismatch: got %v want %v", got, want)
		}
		if got != nil && (got.Tenant != want.Tenant || got.Repo != want.Repo || got.Perm != want.Perm) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	}
}

// TestPublicKeyCallback_RateLimitsRepeatedBadKeys verifies that after Burst
// consecutive credential-error responses, the limiter short-circuits further
// attempts without consulting the store.
func TestPublicKeyCallback_RateLimitsRepeatedBadKeys(t *testing.T) {
	var verifyCalls atomic.Int32
	store := &fakeStore{verify: func(c auth.Credential) (*auth.Actor, string, *auth.Scope, error) {
		verifyCalls.Add(1)
		return nil, "", nil, auth.ErrInvalidCredential
	}}
	opts := newTestServerOpts(t, store)
	opts.Limiter = ratelimit.NewLimiter(ratelimit.Config{
		Burst:           3,
		RefillPerMinute: 0, // no decay during test
		SweepInterval:   24 * time.Hour,
	})
	t.Cleanup(opts.Limiter.Close)
	s, err := NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	pub := mustReadAuthorizedKey(t, "testdata/ed25519.pub")
	meta := stubConnMetadata{user: "git"}

	// First 3 attempts: hit the store, return ErrInvalidCredential, each
	// MarkFailure increments the IP bucket. After the 3rd MarkFailure the
	// bucket equals Burst, so the next Check will refuse before touching
	// the store.
	for i := 0; i < 3; i++ {
		_, err := s.publicKeyCallback(meta, pub)
		if !errors.Is(err, auth.ErrInvalidCredential) {
			t.Fatalf("attempt %d: got %v, want ErrInvalidCredential", i+1, err)
		}
	}
	if got := verifyCalls.Load(); got != 3 {
		t.Fatalf("after 3 bad attempts, VerifyCredential called %d times; want 3", got)
	}

	// 4th attempt: rate-limited before store call.
	_, err = s.publicKeyCallback(meta, pub)
	if err == nil {
		t.Fatal("4th attempt: expected rate-limit error, got nil")
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("4th attempt: error = %q, want 'rate limited'", err.Error())
	}
	if got := verifyCalls.Load(); got != 3 {
		t.Fatalf("after rate-limit, VerifyCredential called %d times; want still 3", got)
	}

	// 5th attempt: still limited, store still untouched.
	_, err = s.publicKeyCallback(meta, pub)
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("5th attempt: error = %v, want 'rate limited'", err)
	}
	if got := verifyCalls.Load(); got != 3 {
		t.Fatalf("after second rate-limit, VerifyCredential called %d times; want still 3", got)
	}
}

// TestPublicKeyCallback_NilLimiterIsNoop verifies that a nil Limiter
// disables rate limiting entirely; bad-key attempts continue to return
// the underlying store error forever.
func TestPublicKeyCallback_NilLimiterIsNoop(t *testing.T) {
	var verifyCalls atomic.Int32
	store := &fakeStore{verify: func(c auth.Credential) (*auth.Actor, string, *auth.Scope, error) {
		verifyCalls.Add(1)
		return nil, "", nil, auth.ErrInvalidCredential
	}}
	opts := newTestServerOpts(t, store)
	opts.Limiter = nil
	s, err := NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	pub := mustReadAuthorizedKey(t, "testdata/ed25519.pub")
	meta := stubConnMetadata{user: "git"}

	const N = 100
	for i := 0; i < N; i++ {
		_, err := s.publicKeyCallback(meta, pub)
		if !errors.Is(err, auth.ErrInvalidCredential) {
			t.Fatalf("attempt %d: got %v, want ErrInvalidCredential", i+1, err)
		}
		if err != nil && strings.Contains(err.Error(), "rate limited") {
			t.Fatalf("attempt %d: got rate-limit error with nil Limiter", i+1)
		}
	}
	if got := verifyCalls.Load(); got != N {
		t.Fatalf("VerifyCredential called %d times; want %d", got, N)
	}
}

// TestPublicKeyCallback_MarkSuccessResetsBucket verifies that a successful
// key verification clears prior failures, so subsequent attempts from the
// same IP aren't penalized.
func TestPublicKeyCallback_MarkSuccessResetsBucket(t *testing.T) {
	// Verify sequence: fail, fail, succeed (one-shot), then fail forever.
	// After the success, the bucket should be reset to 0 so we get a full
	// fresh Burst window before rate-limiting kicks in.
	var attempt atomic.Int32
	store := &fakeStore{verify: func(c auth.Credential) (*auth.Actor, string, *auth.Scope, error) {
		n := attempt.Add(1)
		if n == 3 {
			return &auth.Actor{UserID: "u1", Name: "alice"}, "kabc", nil, nil
		}
		return nil, "", nil, auth.ErrInvalidCredential
	}}
	opts := newTestServerOpts(t, store)
	opts.Limiter = ratelimit.NewLimiter(ratelimit.Config{
		Burst:           3,
		RefillPerMinute: 0,
		SweepInterval:   24 * time.Hour,
	})
	t.Cleanup(opts.Limiter.Close)
	s, err := NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	pub := mustReadAuthorizedKey(t, "testdata/ed25519.pub")
	meta := stubConnMetadata{user: "git"}

	// 2 failures (bucket = 2, under Burst=3 so still allowed).
	for i := 0; i < 2; i++ {
		_, err := s.publicKeyCallback(meta, pub)
		if !errors.Is(err, auth.ErrInvalidCredential) {
			t.Fatalf("attempt %d: got %v, want ErrInvalidCredential", i+1, err)
		}
	}
	// 3rd attempt succeeds — MarkSuccess clears the bucket.
	perms, err := s.publicKeyCallback(meta, pub)
	if err != nil {
		t.Fatalf("success attempt: %v", err)
	}
	if perms.Extensions["actor_id"] != "u1" {
		t.Fatalf("actor_id = %q", perms.Extensions["actor_id"])
	}
	// Now fail 3 more times; all should hit the store and return
	// ErrInvalidCredential (bucket was reset, full Burst available again).
	for i := 0; i < 3; i++ {
		_, err := s.publicKeyCallback(meta, pub)
		if !errors.Is(err, auth.ErrInvalidCredential) {
			t.Fatalf("post-reset attempt %d: got %v, want ErrInvalidCredential", i+1, err)
		}
	}
	// 4th post-reset attempt: bucket full again, rate-limited.
	_, err = s.publicKeyCallback(meta, pub)
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("post-reset 4th attempt: error = %v, want 'rate limited'", err)
	}
}

// TestPublicKeyCallback_NonCredentialErrorNotCounted verifies that internal
// errors (DB unreachable, context canceled, etc.) do NOT increment the
// rate-limit bucket. Otherwise a flaky backend could lock out legitimate
// clients.
func TestPublicKeyCallback_NonCredentialErrorNotCounted(t *testing.T) {
	sentinel := errors.New("db unreachable")
	store := &fakeStore{verify: func(c auth.Credential) (*auth.Actor, string, *auth.Scope, error) {
		return nil, "", nil, sentinel
	}}
	opts := newTestServerOpts(t, store)
	opts.Limiter = ratelimit.NewLimiter(ratelimit.Config{
		Burst:           3,
		RefillPerMinute: 0,
		SweepInterval:   24 * time.Hour,
	})
	t.Cleanup(opts.Limiter.Close)
	s, err := NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	pub := mustReadAuthorizedKey(t, "testdata/ed25519.pub")
	meta := stubConnMetadata{user: "git"}

	// 10 internal errors — none should count toward the bucket.
	for i := 0; i < 10; i++ {
		_, err := s.publicKeyCallback(meta, pub)
		if !errors.Is(err, sentinel) {
			t.Fatalf("attempt %d: got %v, want sentinel", i+1, err)
		}
		if err != nil && strings.Contains(err.Error(), "rate limited") {
			t.Fatalf("attempt %d: internal error converted to rate-limit", i+1)
		}
	}
	// Bucket should still have full capacity. Use CheckDetailed directly
	// against the limiter to confirm.
	ip := sshRemoteIP(meta)
	if allowed, _, _ := opts.Limiter.CheckDetailed(ip, ""); !allowed {
		t.Fatal("internal errors counted toward rate-limit bucket; want bucket untouched")
	}
}

// TestSSHRemoteIP_StripsPort verifies the IP-extraction helper handles the
// common cases.
func TestSSHRemoteIP_StripsPort(t *testing.T) {
	cases := []struct {
		name string
		addr net.Addr
		want string
	}{
		{"ipv4", &net.TCPAddr{IP: net.ParseIP("203.0.113.5"), Port: 22}, "203.0.113.5"},
		{"ipv6", &net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 22}, "2001:db8::1"},
		{"localhost", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 65535}, "127.0.0.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := stubConnMetadataAddr{addr: tc.addr}
			got := sshRemoteIP(meta)
			if got != tc.want {
				t.Fatalf("sshRemoteIP(%s) = %q, want %q", tc.addr, got, tc.want)
			}
		})
	}
}

// stubConnMetadataAddr is a stubConnMetadata variant with a configurable
// RemoteAddr for the sshRemoteIP tests.
type stubConnMetadataAddr struct {
	addr net.Addr
}

func (m stubConnMetadataAddr) User() string          { return "git" }
func (m stubConnMetadataAddr) SessionID() []byte     { return nil }
func (m stubConnMetadataAddr) ClientVersion() []byte { return []byte("test") }
func (m stubConnMetadataAddr) ServerVersion() []byte { return []byte("bucketvcs-test") }
func (m stubConnMetadataAddr) RemoteAddr() net.Addr  { return m.addr }
func (m stubConnMetadataAddr) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0}
}

// stubConnMetadata implements ssh.ConnMetadata for tests.
type stubConnMetadata struct {
	user string
}

func (m stubConnMetadata) User() string          { return m.user }
func (m stubConnMetadata) SessionID() []byte     { return nil }
func (m stubConnMetadata) ClientVersion() []byte { return []byte("test") }
func (m stubConnMetadata) ServerVersion() []byte { return []byte("bucketvcs-test") }
func (m stubConnMetadata) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0}
}
func (m stubConnMetadata) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0}
}

// Satisfy the ssh import so it doesn't get dropped by goimports.
var _ ssh.ServerConfig
