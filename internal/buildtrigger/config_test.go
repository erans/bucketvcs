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
