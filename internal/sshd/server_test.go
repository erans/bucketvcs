package sshd

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/bucketvcs/bucketvcs/internal/auth"
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
