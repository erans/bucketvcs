package buildtrigger

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

func TestEdit_UpdatesSafeFieldsLeavesKindAndSecret(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestSvc(t)

	created, err := s.Create(ctx, TriggerInput{
		Tenant: "acme", Repo: "app", Name: "ci", Kind: KindCloudBuild,
		Config: Config{URL: "https://example.com/h"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	updated, err := s.Edit(ctx, created.ID, EditInput{
		Name:        "ci-renamed",
		RefInclude:  []string{"refs/heads/main"},
		RefExclude:  []string{},
		TokenMode:   TokenInject,
		TokenScopes: auth.ScopeRepoRead,
		TokenTTL:    30 * time.Minute,
		Active:      false,
	})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if updated.Name != "ci-renamed" {
		t.Errorf("name not updated: %q", updated.Name)
	}
	if updated.Kind != KindCloudBuild {
		t.Errorf("kind changed: %q", updated.Kind)
	}
	if updated.Active {
		t.Errorf("active not updated")
	}
	if len(updated.RefInclude) != 1 || updated.RefInclude[0] != "refs/heads/main" {
		t.Errorf("ref_include not updated: %v", updated.RefInclude)
	}
	if updated.TokenTTL != 30*time.Minute {
		t.Errorf("ttl not updated: %v", updated.TokenTTL)
	}
	if updated.SecretPreview == "" {
		t.Errorf("secret preview lost after edit")
	}
}

func TestEdit_DuplicateNameConflict(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestSvc(t)
	if _, err := s.Create(ctx, TriggerInput{Tenant: "acme", Repo: "app", Name: "a", Kind: KindGeneric, Config: Config{URL: "https://x"}}); err != nil {
		t.Fatal(err)
	}
	b, err := s.Create(ctx, TriggerInput{Tenant: "acme", Repo: "app", Name: "b", Kind: KindGeneric, Config: Config{URL: "https://y"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Edit(ctx, b.ID, EditInput{Name: "a", RefInclude: []string{}, RefExclude: []string{}, Active: true})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
}

func TestEdit_TTLOverCeilingRejected(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestSvc(t)
	c, err := s.Create(ctx, TriggerInput{Tenant: "acme", Repo: "app", Name: "a", Kind: KindGeneric, Config: Config{URL: "https://x"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Edit(ctx, c.ID, EditInput{Name: "a", RefInclude: []string{}, RefExclude: []string{}, TokenTTL: 2 * time.Hour, Active: true})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput, got %v", err)
	}
}

func TestEdit_NotFound(t *testing.T) {
	s, _ := newTestSvc(t)
	_, err := s.Edit(context.Background(), "bvbt_missing", EditInput{Name: "x", RefInclude: []string{}, RefExclude: []string{}, Active: true})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestEdit_NegativeTTLRejected(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestSvc(t)
	c, err := s.Create(ctx, TriggerInput{Tenant: "acme", Repo: "app", Name: "a", Kind: KindGeneric, Config: Config{URL: "https://x"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Edit(ctx, c.ID, EditInput{Name: "a", RefInclude: []string{}, RefExclude: []string{}, TokenTTL: -time.Second, Active: true})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput, got %v", err)
	}
}

func TestEdit_EmptyNameRejected(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestSvc(t)
	c, err := s.Create(ctx, TriggerInput{Tenant: "acme", Repo: "app", Name: "a", Kind: KindGeneric, Config: Config{URL: "https://x"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Edit(ctx, c.ID, EditInput{Name: "", RefInclude: []string{}, RefExclude: []string{}, Active: true})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput, got %v", err)
	}
}
