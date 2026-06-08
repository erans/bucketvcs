package buildtrigger

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
)

// newTestSvc opens a fresh on-disk authdb with migrations applied, seeds the
// repo acme/app so the build_triggers FK is satisfiable, and returns a Service
// bound to it. Mirrors the open/seed shape used by webhooks/service_test.go.
func newTestSvc(t *testing.T) (*Service, sqlitestore.Querier) {
	t.Helper()
	tmp := t.TempDir()
	store, err := sqlitestore.Open(filepath.Join(tmp, "auth.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := store.DB()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO repos (tenant, name, public_read, created_at)
		 VALUES (?, ?, 0, strftime('%s','now'))`,
		"acme", "app",
	); err != nil {
		t.Fatalf("seed repo row: %v", err)
	}
	return New(db), db
}

func TestStore_CreateListGetRemove(t *testing.T) {
	svc, _ := newTestSvc(t)
	ctx := context.Background()
	tr, err := svc.Create(ctx, TriggerInput{
		Tenant: "acme", Repo: "app", Name: "main-cb", Kind: KindCloudBuild,
		Config:      Config{URL: "https://cloudbuild.example/x:webhook?key=k&secret=s"},
		RefInclude:  []string{"refs/heads/main"},
		TokenMode:   TokenNone,
		TokenScopes: auth.ScopeRepoRead | auth.ScopeLFSRead,
		TokenTTL:    15 * time.Minute,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if tr.Secret == "" {
		t.Fatal("expected generated secret on create")
	}
	got, err := svc.List(ctx, "acme", "app")
	if err != nil || len(got) != 1 {
		t.Fatalf("list: %v len=%d", err, len(got))
	}
	if got[0].Secret != "" || got[0].SecretPreview == "" {
		t.Fatal("list must hide secret, show preview")
	}
	if _, err := svc.Create(ctx, TriggerInput{
		Tenant: "acme", Repo: "app", Name: "main-cb", Kind: KindGeneric, Config: Config{URL: "https://x"},
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
	if err := svc.Disable(ctx, tr.ID); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := svc.Remove(ctx, tr.ID); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := svc.Remove(ctx, tr.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestStore_GetRoundTrip(t *testing.T) {
	svc, _ := newTestSvc(t)
	ctx := context.Background()
	tr, err := svc.Create(ctx, TriggerInput{
		Tenant: "acme", Repo: "app", Name: "cb", Kind: KindCodeBuild,
		Config:     Config{AWSRegion: "us-east-1", AWSProject: "app-release", AWSConnector: "default"},
		RefInclude: []string{"refs/tags/v*"}, RefExclude: []string{"refs/tags/v0.*"},
		TokenMode: TokenInject, TokenScopes: auth.ScopeRepoRead, TokenTTL: 10 * time.Minute,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := svc.Get(ctx, tr.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Kind != KindCodeBuild || got.Config.AWSProject != "app-release" ||
		got.Config.AWSConnector != "default" ||
		len(got.RefInclude) != 1 || len(got.RefExclude) != 1 ||
		got.TokenMode != TokenInject || got.TokenScopes != auth.ScopeRepoRead ||
		got.TokenTTL != 10*time.Minute || !got.Active {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestStore_CreateValidation(t *testing.T) {
	svc, _ := newTestSvc(t)
	ctx := context.Background()
	bad := []TriggerInput{
		{Tenant: "", Repo: "app", Name: "n", Kind: KindGeneric, Config: Config{URL: "https://x"}},
		{Tenant: "acme", Repo: "app", Name: "n", Kind: "bogus", Config: Config{URL: "https://x"}},
		{Tenant: "acme", Repo: "app", Name: "n", Kind: KindGeneric, Config: Config{URL: ""}},
		{Tenant: "acme", Repo: "app", Name: "n", Kind: KindCodeBuild, Config: Config{AWSProject: ""}},
		{Tenant: "acme", Repo: "app", Name: "n", Kind: KindGeneric, Config: Config{URL: "https://x"}, RefInclude: []string{"["}},
		{Tenant: "acme", Repo: "app", Name: "n", Kind: KindGeneric, Config: Config{URL: "https://x"}, TokenTTL: 2 * time.Hour},
		{Tenant: "acme", Repo: "app", Name: "bad name", Kind: KindGeneric, Config: Config{URL: "https://x"}},
		// empty Repo
		{Tenant: "acme", Repo: "", Name: "n", Kind: KindGeneric, Config: Config{URL: "https://x"}},
		// empty Name
		{Tenant: "acme", Repo: "app", Name: "", Kind: KindGeneric, Config: Config{URL: "https://x"}},
		// negative TokenTTL
		{Tenant: "acme", Repo: "app", Name: "n", Kind: KindGeneric, Config: Config{URL: "https://x"}, TokenTTL: -1 * time.Second},
		// invalid TokenMode
		{Tenant: "acme", Repo: "app", Name: "n", Kind: KindGeneric, Config: Config{URL: "https://x"}, TokenMode: "bogus"},
		// codebuild with AWSProject set but AWSRegion empty
		{Tenant: "acme", Repo: "app", Name: "n", Kind: KindCodeBuild, Config: Config{AWSProject: "p", AWSRegion: ""}},
	}
	for i, in := range bad {
		if _, err := svc.Create(ctx, in); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("case %d: want ErrInvalidInput, got %v", i, err)
		}
	}
}

func TestStore_GetNotFound(t *testing.T) {
	svc, _ := newTestSvc(t)
	ctx := context.Background()
	_, err := svc.Get(ctx, "bvbt_doesnotexist")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestStore_EnableDisableNotFound(t *testing.T) {
	svc, _ := newTestSvc(t)
	ctx := context.Background()
	const missing = "bvbt_nosuchid"
	if err := svc.Enable(ctx, missing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Enable: want ErrNotFound, got %v", err)
	}
	if err := svc.Disable(ctx, missing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Disable: want ErrNotFound, got %v", err)
	}
}

func TestStore_DefaultsTokenModeAndScopes(t *testing.T) {
	svc, _ := newTestSvc(t)
	ctx := context.Background()
	// codebuild defaults token_mode=inject; generic defaults none; empty scopes default repo:read,lfs:read; ttl 0 -> 15m.
	cb, _ := svc.Create(ctx, TriggerInput{Tenant: "acme", Repo: "app", Name: "cb", Kind: KindCodeBuild,
		Config: Config{AWSRegion: "us-east-1", AWSProject: "p"}})
	if cb.TokenMode != TokenInject {
		t.Fatalf("codebuild default token_mode=%q want inject", cb.TokenMode)
	}
	gen, _ := svc.Create(ctx, TriggerInput{Tenant: "acme", Repo: "app", Name: "gen", Kind: KindGeneric,
		Config: Config{URL: "https://x"}})
	if gen.TokenMode != TokenNone {
		t.Fatalf("generic default token_mode=%q want none", gen.TokenMode)
	}
	if gen.TokenScopes != (auth.ScopeRepoRead | auth.ScopeLFSRead) {
		t.Fatalf("default scopes=%v", gen.TokenScopes)
	}
	if gen.TokenTTL != 15*time.Minute {
		t.Fatalf("default ttl=%v", gen.TokenTTL)
	}
}
