package policy_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/policy"
)

func TestService_AddListRemovePathRule(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db)
	ctx := context.Background()

	got, err := svc.ListPathRules(ctx, "acme", "site")
	if err != nil {
		t.Fatalf("ListPathRules empty: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ListPathRules empty: len=%d, want 0", len(got))
	}

	if err := svc.AddPathRule(ctx, policy.ProtectedPath{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/main",
		PathPattern:    "secrets/**",
	}); err != nil {
		t.Fatalf("AddPathRule secrets: %v", err)
	}
	if err := svc.AddPathRule(ctx, policy.ProtectedPath{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/main",
		PathPattern:    ".github/workflows/*",
	}); err != nil {
		t.Fatalf("AddPathRule workflows: %v", err)
	}

	got, err = svc.ListPathRules(ctx, "acme", "site")
	if err != nil {
		t.Fatalf("ListPathRules: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListPathRules len=%d, want 2; got=%+v", len(got), got)
	}
	if got[0].PathPattern != ".github/workflows/*" {
		t.Errorf("got[0].PathPattern=%q, want .github/workflows/*", got[0].PathPattern)
	}
	if got[1].PathPattern != "secrets/**" {
		t.Errorf("got[1].PathPattern=%q, want secrets/**", got[1].PathPattern)
	}
	if got[0].CreatedAt.IsZero() {
		t.Errorf("CreatedAt is zero")
	}

	if err := svc.RemovePathRule(ctx, "acme", "site",
		"refs/heads/main", ".github/workflows/*"); err != nil {
		t.Fatalf("RemovePathRule: %v", err)
	}
	got, _ = svc.ListPathRules(ctx, "acme", "site")
	if len(got) != 1 || got[0].PathPattern != "secrets/**" {
		t.Errorf("after Remove: %+v, want only secrets/**", got)
	}

	err = svc.RemovePathRule(ctx, "acme", "site",
		"refs/heads/main", "nonexistent/**")
	if !errors.Is(err, policy.ErrNotFound) {
		t.Errorf("RemovePathRule non-existent: err=%v, want ErrNotFound", err)
	}
}

func TestService_AddPathRuleIdempotent(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db)
	ctx := context.Background()
	ref := policy.ProtectedPath{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/main",
		PathPattern:    "secrets/**",
	}
	if err := svc.AddPathRule(ctx, ref); err != nil {
		t.Fatalf("Add first: %v", err)
	}
	if err := svc.AddPathRule(ctx, ref); err != nil {
		t.Fatalf("Add idempotent: %v", err)
	}
	got, _ := svc.ListPathRules(ctx, "acme", "site")
	if len(got) != 1 {
		t.Fatalf("len=%d, want 1 (idempotent re-add)", len(got))
	}
}

func TestService_CheckPaths_NoRules(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db)
	if err := svc.CheckPaths(context.Background(), "acme", "site",
		"refs/heads/main", []string{"any/file.txt"}); err != nil {
		t.Errorf("CheckPaths no rules: %v, want nil", err)
	}
}

func TestService_CheckPaths_Rejects(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db)
	ctx := context.Background()
	if err := svc.AddPathRule(ctx, policy.ProtectedPath{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/main",
		PathPattern:    "secrets/**",
	}); err != nil {
		t.Fatalf("AddPathRule: %v", err)
	}
	err := svc.CheckPaths(ctx, "acme", "site", "refs/heads/main",
		[]string{"README.md", "secrets/api.key"})
	if err == nil {
		t.Fatalf("CheckPaths: nil error; want PolicyError")
	}
	var perr *policy.PolicyError
	if !errors.As(err, &perr) {
		t.Fatalf("CheckPaths: err type %T, want *PolicyError", err)
	}
	if perr.Reason != "blocked_path" {
		t.Errorf("Reason=%q, want blocked_path", perr.Reason)
	}
	if perr.MatchedPattern != "secrets/**" {
		t.Errorf("MatchedPattern=%q, want secrets/**", perr.MatchedPattern)
	}
	if perr.MatchedPath != "secrets/api.key" {
		t.Errorf("MatchedPath=%q, want secrets/api.key", perr.MatchedPath)
	}
	if perr.MetricOutcome() != "blocked_path" {
		t.Errorf("MetricOutcome=%q, want blocked_path", perr.MetricOutcome())
	}
}

func TestService_CheckPaths_RefPatternFilters(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db)
	ctx := context.Background()
	if err := svc.AddPathRule(ctx, policy.ProtectedPath{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/main",
		PathPattern:    "secrets/**",
	}); err != nil {
		t.Fatalf("AddPathRule: %v", err)
	}
	if err := svc.CheckPaths(ctx, "acme", "site", "refs/heads/feature/x",
		[]string{"secrets/api.key"}); err != nil {
		t.Errorf("CheckPaths feature branch: %v, want nil", err)
	}
}

func TestService_CheckPaths_FirstMatchAlphabetical(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db)
	ctx := context.Background()
	if err := svc.AddPathRule(ctx, policy.ProtectedPath{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/main",
		PathPattern:    "secrets/**",
	}); err != nil {
		t.Fatalf("AddPathRule 1: %v", err)
	}
	if err := svc.AddPathRule(ctx, policy.ProtectedPath{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/main",
		PathPattern:    "**/.env",
	}); err != nil {
		t.Fatalf("AddPathRule 2: %v", err)
	}
	err := svc.CheckPaths(ctx, "acme", "site", "refs/heads/main",
		[]string{"secrets/.env"})
	if err == nil {
		t.Fatalf("CheckPaths: nil; want rejection")
	}
	var perr *policy.PolicyError
	errors.As(err, &perr)
	if perr.MatchedPattern != "**/.env" {
		t.Errorf("MatchedPattern=%q, want **/.env (first alphabetical)", perr.MatchedPattern)
	}
}

// TestService_AddPathRule_RejectsBadRefnamePattern locks in the M16
// round-1 fix (L4): AddPathRule validates refname_pattern at write
// time via stdlib path.Match. Without the gate, a malformed
// refname_pattern would silently no-op in CheckPaths — the rule would
// exist in the table but never match any refname, giving operators no
// signal that they typoed the pattern.
func TestService_AddPathRule_RejectsBadRefnamePattern(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db)
	err := svc.AddPathRule(context.Background(), policy.ProtectedPath{
		Tenant:         "acme",
		Repo:           "site",
		RefnamePattern: "refs/heads/[unclosed",
		PathPattern:    "secrets/**",
	})
	if !errors.Is(err, policy.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for bad refname_pattern, got %v", err)
	}
}
