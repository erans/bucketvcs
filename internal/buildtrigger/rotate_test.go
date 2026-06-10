package buildtrigger

import (
	"context"
	"errors"
	"testing"
)

func TestRotateSecret_GenericReturnsNewSecret(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestSvc(t)
	c, err := s.Create(ctx, TriggerInput{Tenant: "acme", Repo: "app", Name: "a", Kind: KindGeneric, Config: Config{URL: "https://x"}})
	if err != nil {
		t.Fatal(err)
	}
	oldPreview := c.Secret[:6]
	secret, err := s.RotateSecret(ctx, c.ID)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if secret == "" || secret[:6] == oldPreview {
		t.Errorf("secret not rotated: old=%s new=%s", oldPreview, secret)
	}
	got, err := s.Get(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SecretPreview != secret[:6] {
		t.Errorf("stored preview %q != new secret prefix %q", got.SecretPreview, secret[:6])
	}
}

func TestRotateSecret_CodeBuildRejected(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestSvc(t)
	c, err := s.Create(ctx, TriggerInput{Tenant: "acme", Repo: "app", Name: "a", Kind: KindCodeBuild, Config: Config{AWSRegion: "us-east-1", AWSProject: "p"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RotateSecret(ctx, c.ID); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("want ErrInvalidInput, got %v", err)
	}
}

func TestRotateSecret_NotFound(t *testing.T) {
	s, _ := newTestSvc(t)
	if _, err := s.RotateSecret(context.Background(), "bvbt_x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
