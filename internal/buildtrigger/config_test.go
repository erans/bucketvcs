package buildtrigger

import (
	"context"
	"testing"
	"time"
)

func TestParseServeConfig(t *testing.T) {
	yml := []byte(`
build:
  defaults:
    token_ttl: 15m
    token_scopes: ["repo:read","lfs:read"]
    audience: https://bucketvcs.example
  aws_connectors:
    default:
      region: us-east-1
      profile: bucketvcs-codebuild
`)
	cfg, err := ParseServeConfig(yml)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Build.Defaults.TokenTTL != 15*time.Minute {
		t.Fatalf("ttl=%v", cfg.Build.Defaults.TokenTTL)
	}
	c, ok := cfg.Build.AWSConnectors["default"]
	if !ok || c.Region != "us-east-1" || c.Profile != "bucketvcs-codebuild" {
		t.Fatalf("connector: %+v", cfg.Build.AWSConnectors)
	}
}

func TestParseServeConfig_BadTTL(t *testing.T) {
	if _, err := ParseServeConfig([]byte("build:\n  defaults:\n    token_ttl: notaduration\n")); err == nil {
		t.Fatal("expected error for bad token_ttl")
	}
}

func TestApply_Reconcile(t *testing.T) {
	svc, _ := newTestSvc(t)
	ctx := context.Background()
	yml := []byte(`
triggers:
  - tenant: acme
    repo: app
    name: main
    kind: cloudbuild
    url: https://cb.example/x:webhook?key=k&secret=s
    ref_include: ["refs/heads/main"]
    token_mode: none
`)
	res, err := Apply(ctx, svc, yml, false)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Created != 1 {
		t.Fatalf("created=%d", res.Created)
	}
	res2, err := Apply(ctx, svc, yml, false)
	if err != nil {
		t.Fatalf("apply2: %v", err)
	}
	if res2.Created != 0 || res2.Updated != 1 {
		t.Fatalf("second apply: %+v", res2)
	}
}

func TestApply_Prune(t *testing.T) {
	svc, _ := newTestSvc(t)
	ctx := context.Background()
	// create two triggers via apply, then apply a doc with only one + prune -> the other removed.
	two := []byte(`
triggers:
  - {tenant: acme, repo: app, name: a, kind: generic, url: "https://x", token_mode: none}
  - {tenant: acme, repo: app, name: b, kind: generic, url: "https://x", token_mode: none}
`)
	if _, err := Apply(ctx, svc, two, false); err != nil {
		t.Fatal(err)
	}
	one := []byte(`
triggers:
  - {tenant: acme, repo: app, name: a, kind: generic, url: "https://x", token_mode: none}
`)
	res, err := Apply(ctx, svc, one, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Pruned != 1 {
		t.Fatalf("pruned=%d want 1", res.Pruned)
	}
	got, _ := svc.List(ctx, "acme", "app")
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("after prune: %+v", got)
	}
}

func TestApply_PruneRepoIsolation(t *testing.T) {
	svc, db := newTestSvc(t)
	ctx := context.Background()

	// Seed a second repo acme/other in the repos table.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO repos (tenant, name, public_read, created_at)
		 VALUES (?, ?, 0, strftime('%s','now'))`,
		"acme", "other",
	); err != nil {
		t.Fatalf("seed acme/other: %v", err)
	}

	// Create a trigger in each repo via Apply.
	seed := []byte(`
triggers:
  - {tenant: acme, repo: app,   name: app-trigger,   kind: generic, url: "https://app",   token_mode: none}
  - {tenant: acme, repo: other, name: other-trigger,  kind: generic, url: "https://other", token_mode: none}
`)
	if _, err := Apply(ctx, svc, seed, false); err != nil {
		t.Fatalf("seed apply: %v", err)
	}

	// Apply a doc that mentions only acme/app (one trigger still listed) with prune=true.
	appOnly := []byte(`
triggers:
  - {tenant: acme, repo: app, name: app-trigger, kind: generic, url: "https://app", token_mode: none}
`)
	res, err := Apply(ctx, svc, appOnly, true)
	if err != nil {
		t.Fatalf("apply prune: %v", err)
	}

	// acme/app trigger is still listed → no prune for app; Pruned must be 0.
	if res.Pruned != 0 {
		t.Fatalf("pruned=%d want 0 (app trigger is still declared)", res.Pruned)
	}

	// acme/other trigger must still be present — prune must not touch it.
	otherTriggers, err := svc.List(ctx, "acme", "other")
	if err != nil {
		t.Fatalf("list acme/other: %v", err)
	}
	if len(otherTriggers) != 1 || otherTriggers[0].Name != "other-trigger" {
		t.Fatalf("acme/other triggers after prune: %+v (want 1 trigger named other-trigger)", otherTriggers)
	}
}

func TestApply_BadEntryErrors(t *testing.T) {
	svc, _ := newTestSvc(t)
	ctx := context.Background()

	// A trigger with a malformed token_ttl should cause Apply to return an error.
	bad := []byte(`
triggers:
  - {tenant: acme, repo: app, name: bad-ttl, kind: generic, url: "https://x", token_mode: none, token_ttl: "notaduration"}
`)
	res, err := Apply(ctx, svc, bad, false)
	if err == nil {
		t.Fatal("expected error for bad token_ttl, got nil")
	}
	// Apply must have aborted before creating anything.
	if res.Created != 0 || res.Updated != 0 {
		t.Fatalf("expected zero Created/Updated on error, got %+v", res)
	}

	// Confirm nothing was inserted.
	triggers, listErr := svc.List(ctx, "acme", "app")
	if listErr != nil {
		t.Fatalf("list: %v", listErr)
	}
	if len(triggers) != 0 {
		t.Fatalf("expected no triggers after aborted apply, got %d", len(triggers))
	}
}

func TestApply_UpdateReplacesConfig(t *testing.T) {
	svc, _ := newTestSvc(t)
	ctx := context.Background()

	first := []byte(`
triggers:
  - {tenant: acme, repo: app, name: web, kind: generic, url: "https://old", token_mode: none}
`)
	res1, err := Apply(ctx, svc, first, false)
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if res1.Created != 1 {
		t.Fatalf("first apply created=%d want 1", res1.Created)
	}

	second := []byte(`
triggers:
  - {tenant: acme, repo: app, name: web, kind: generic, url: "https://new", token_mode: none}
`)
	res2, err := Apply(ctx, svc, second, false)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if res2.Updated != 1 || res2.Created != 0 {
		t.Fatalf("second apply: %+v want Updated=1 Created=0", res2)
	}

	// The stored URL must reflect the new value.
	triggers, err := svc.List(ctx, "acme", "app")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(triggers) != 1 {
		t.Fatalf("expected 1 trigger, got %d", len(triggers))
	}
	if triggers[0].Config.URL != "https://new" {
		t.Fatalf("URL=%q want https://new", triggers[0].Config.URL)
	}
}
