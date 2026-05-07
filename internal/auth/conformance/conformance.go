package conformance

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// SeedUserFn lets the conformance suite stand up baseline users/tokens/repos
// without depending on a specific Store impl's CRUD methods. The factory
// returns a fresh Store and a Seeder bound to it; the Seeder applies the
// minimum operations the conformance tests need.
type Seeder interface {
	CreateUser(ctx context.Context, name string, isAdmin bool) (userID string)
	CreateToken(ctx context.Context, userID, tokenID, secretHash string, expiresAt *int64)
	RevokeToken(ctx context.Context, tokenID string)
	SetUserDisabled(ctx context.Context, name string, disabled bool)
	RegisterRepo(ctx context.Context, tenant, repo string)
	SetRepoPublic(ctx context.Context, tenant, repo string, public bool)
	Grant(ctx context.Context, userName, tenant, repo, perm string)
}

// Factory builds a fresh (Store, Seeder) pair for each test.
type Factory func(t *testing.T) (auth.Store, Seeder)

// Run executes the full conformance suite.
func Run(t *testing.T, factory Factory) {
	t.Run("VerifyCredential_RejectsUnknownTokenID", func(t *testing.T) {
		s, _ := factory(t)
		defer s.Close()
		tok, _, _, _ := auth.GenerateToken()
		_, _, err := s.VerifyCredential(context.Background(),
			auth.BasicPassword{Username: "alice", Password: tok})
		mustErrIs(t, err, auth.ErrInvalidCredential)
	})

	t.Run("VerifyCredential_RejectsBadSecret", func(t *testing.T) {
		s, sd := factory(t)
		defer s.Close()
		ctx := context.Background()
		uid := sd.CreateUser(ctx, "alice", false)
		hash, _ := auth.HashSecret("the-real-secret")
		_, id, _, _ := auth.GenerateToken()
		sd.CreateToken(ctx, uid, id, hash, nil)
		bad := "bvts_" + id + "_" + strings.Repeat("A", 52)
		_, _, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "alice", Password: bad})
		mustErrIs(t, err, auth.ErrInvalidCredential)
	})

	t.Run("VerifyCredential_RejectsExpired", func(t *testing.T) {
		s, sd := factory(t)
		defer s.Close()
		ctx := context.Background()
		uid := sd.CreateUser(ctx, "alice", false)
		tok, id, secret, _ := auth.GenerateToken()
		hash, _ := auth.HashSecret(secret)
		past := time.Now().Add(-time.Hour).Unix()
		sd.CreateToken(ctx, uid, id, hash, &past)
		_, _, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "alice", Password: tok})
		mustErrIs(t, err, auth.ErrTokenExpired)
	})

	t.Run("VerifyCredential_RejectsRevoked", func(t *testing.T) {
		s, sd := factory(t)
		defer s.Close()
		ctx := context.Background()
		uid := sd.CreateUser(ctx, "alice", false)
		tok, id, secret, _ := auth.GenerateToken()
		hash, _ := auth.HashSecret(secret)
		sd.CreateToken(ctx, uid, id, hash, nil)
		sd.RevokeToken(ctx, id)
		_, _, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "alice", Password: tok})
		mustErrIs(t, err, auth.ErrTokenRevoked)
	})

	t.Run("VerifyCredential_RejectsDisabled", func(t *testing.T) {
		s, sd := factory(t)
		defer s.Close()
		ctx := context.Background()
		uid := sd.CreateUser(ctx, "alice", false)
		tok, id, secret, _ := auth.GenerateToken()
		hash, _ := auth.HashSecret(secret)
		sd.CreateToken(ctx, uid, id, hash, nil)
		sd.SetUserDisabled(ctx, "alice", true)
		_, _, err := s.VerifyCredential(ctx, auth.BasicPassword{Username: "alice", Password: tok})
		mustErrIs(t, err, auth.ErrUserDisabled)
	})

	t.Run("LookupRepoPerm_NoneForNoGrant", func(t *testing.T) {
		s, sd := factory(t)
		defer s.Close()
		ctx := context.Background()
		uid := sd.CreateUser(ctx, "alice", false)
		sd.RegisterRepo(ctx, "acme", "foo")
		actor := &auth.Actor{UserID: uid, Name: "alice"}
		p, err := s.LookupRepoPerm(ctx, actor, "acme", "foo")
		if err != nil || p != auth.PermNone {
			t.Fatalf("got perm=%v err=%v", p, err)
		}
	})

	t.Run("LookupRepoPerm_GrantedLevel", func(t *testing.T) {
		s, sd := factory(t)
		defer s.Close()
		ctx := context.Background()
		uid := sd.CreateUser(ctx, "alice", false)
		sd.RegisterRepo(ctx, "acme", "foo")
		sd.Grant(ctx, "alice", "acme", "foo", "write")
		actor := &auth.Actor{UserID: uid, Name: "alice"}
		p, _ := s.LookupRepoPerm(ctx, actor, "acme", "foo")
		if p != auth.PermWrite {
			t.Fatalf("perm = %v want PermWrite", p)
		}
	})

	t.Run("LookupRepoPerm_AdminShortCircuits", func(t *testing.T) {
		s, sd := factory(t)
		defer s.Close()
		ctx := context.Background()
		uid := sd.CreateUser(ctx, "root", true)
		sd.RegisterRepo(ctx, "acme", "foo")
		actor := &auth.Actor{UserID: uid, IsAdmin: true}
		p, _ := s.LookupRepoPerm(ctx, actor, "acme", "foo")
		if p != auth.PermAdmin {
			t.Fatalf("perm = %v want PermAdmin", p)
		}
	})

	t.Run("LookupRepoPerm_NilActorIsPermNone", func(t *testing.T) {
		s, _ := factory(t)
		defer s.Close()
		p, err := s.LookupRepoPerm(context.Background(), nil, "acme", "foo")
		if err != nil || p != auth.PermNone {
			t.Fatalf("got perm=%v err=%v", p, err)
		}
	})

	t.Run("GetRepoFlags_NoSuchRepo", func(t *testing.T) {
		s, _ := factory(t)
		defer s.Close()
		_, err := s.GetRepoFlags(context.Background(), "ghost", "x")
		mustErrIs(t, err, auth.ErrNoSuchRepo)
	})

	t.Run("GetRepoFlags_PublicRead", func(t *testing.T) {
		s, sd := factory(t)
		defer s.Close()
		ctx := context.Background()
		sd.RegisterRepo(ctx, "acme", "foo")
		sd.SetRepoPublic(ctx, "acme", "foo", true)
		f, err := s.GetRepoFlags(ctx, "acme", "foo")
		if err != nil {
			t.Fatal(err)
		}
		if !f.PublicRead {
			t.Fatal("expected PublicRead = true")
		}
	})

	t.Run("TouchTokenUsage_IdempotentOnMissing", func(t *testing.T) {
		s, _ := factory(t)
		defer s.Close()
		if err := s.TouchTokenUsage(context.Background(), ""); err != nil {
			t.Fatalf("empty id: %v", err)
		}
		if err := s.TouchTokenUsage(context.Background(), "noSuchABCDE0000000000000"); err != nil {
			t.Fatalf("missing id: %v", err)
		}
	})
}

func mustErrIs(t *testing.T, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Fatalf("got error %v, want %v", got, want)
	}
}
