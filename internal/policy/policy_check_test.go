package policy_test

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/policy"
)

const nullOID = "0000000000000000000000000000000000000000"

// makeBareWithFFChain creates a bare repo with two commits on
// refs/heads/main where commit2 is a descendant of commit1.
// Returns (bareDir, commit1OID, commit2OID).
func makeBareWithFFChain(t *testing.T) (string, string, string) {
	t.Helper()
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "bare.git")
	work := filepath.Join(tmp, "work")

	mustRun := func(dir, name string, args ...string) string {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v in %s failed: %v\n%s", name, args, dir, err, out)
		}
		return string(out)
	}

	mustRun(tmp, "git", "init", "--bare", bare)
	mustRun(tmp, "git", "init", "-q", "-b", "main", work)
	mustRun(work, "git", "config", "user.email", "t@example.com")
	mustRun(work, "git", "config", "user.name", "t")

	mustRun(work, "git", "commit", "--allow-empty", "-qm", "c1")
	commit1 := chomp(mustRun(work, "git", "rev-parse", "HEAD"))
	mustRun(work, "git", "commit", "--allow-empty", "-qm", "c2")
	commit2 := chomp(mustRun(work, "git", "rev-parse", "HEAD"))

	mustRun(work, "git", "push", "-q", bare, "main:refs/heads/main")
	return bare, commit1, commit2
}

func chomp(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}

func TestCheckUpdate_NoRulesIsAccept(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db)
	bare, c1, c2 := makeBareWithFFChain(t)
	if err := svc.CheckUpdate(context.Background(), "acme", "site", bare,
		"refs/heads/main", c1, c2); err != nil {
		t.Errorf("CheckUpdate with no rules: %v, want nil", err)
	}
}

func TestCheckUpdate_NonMatchingPatternIsAccept(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db)
	bare, c1, c2 := makeBareWithFFChain(t)
	_ = svc.Add(context.Background(), policy.ProtectedRef{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/release/*",
		BlockDeletion:  true, BlockForcePush: true,
	})
	if err := svc.CheckUpdate(context.Background(), "acme", "site", bare,
		"refs/heads/main", c1, c2); err != nil {
		t.Errorf("CheckUpdate non-match: %v, want nil", err)
	}
}

func TestCheckUpdate_DeletionBlocked(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db)
	bare, c1, _ := makeBareWithFFChain(t)
	_ = svc.Add(context.Background(), policy.ProtectedRef{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/main",
		BlockDeletion:  true, BlockForcePush: true,
	})
	err := svc.CheckUpdate(context.Background(), "acme", "site", bare,
		"refs/heads/main", c1, nullOID)
	var perr *policy.PolicyError
	if !errors.As(err, &perr) {
		t.Fatalf("CheckUpdate deletion: %T %v, want *PolicyError", err, err)
	}
	if perr.Reason != "deletion blocked" {
		t.Errorf("Reason=%q, want 'deletion blocked'", perr.Reason)
	}
	if perr.MatchedPattern != "refs/heads/main" {
		t.Errorf("MatchedPattern=%q", perr.MatchedPattern)
	}
}

func TestCheckUpdate_DeletionAllowedWhenToggleOff(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db)
	bare, c1, _ := makeBareWithFFChain(t)
	_ = svc.Add(context.Background(), policy.ProtectedRef{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/main",
		BlockDeletion:  false, BlockForcePush: true,
	})
	if err := svc.CheckUpdate(context.Background(), "acme", "site", bare,
		"refs/heads/main", c1, nullOID); err != nil {
		t.Errorf("CheckUpdate deletion with toggle off: %v, want nil", err)
	}
}

func TestCheckUpdate_FastForwardAccepted(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db)
	bare, c1, c2 := makeBareWithFFChain(t)
	_ = svc.Add(context.Background(), policy.ProtectedRef{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/main",
		BlockDeletion:  true, BlockForcePush: true,
	})
	if err := svc.CheckUpdate(context.Background(), "acme", "site", bare,
		"refs/heads/main", c1, c2); err != nil {
		t.Errorf("CheckUpdate FF: %v, want nil", err)
	}
}

func TestCheckUpdate_NonFFRejected(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db)
	bare, c1, c2 := makeBareWithFFChain(t)
	_ = svc.Add(context.Background(), policy.ProtectedRef{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/main",
		BlockDeletion:  true, BlockForcePush: true,
	})
	err := svc.CheckUpdate(context.Background(), "acme", "site", bare,
		"refs/heads/main", c2, c1)
	var perr *policy.PolicyError
	if !errors.As(err, &perr) {
		t.Fatalf("CheckUpdate non-FF: %T %v, want *PolicyError", err, err)
	}
	if perr.Reason != "non-fast-forward push blocked" {
		t.Errorf("Reason=%q, want 'non-fast-forward push blocked'", perr.Reason)
	}
}

func TestCheckUpdate_NonFFAllowedWhenToggleOff(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db)
	bare, c1, c2 := makeBareWithFFChain(t)
	_ = svc.Add(context.Background(), policy.ProtectedRef{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/main",
		BlockDeletion:  true, BlockForcePush: false,
	})
	if err := svc.CheckUpdate(context.Background(), "acme", "site", bare,
		"refs/heads/main", c2, c1); err != nil {
		t.Errorf("CheckUpdate non-FF with toggle off: %v, want nil", err)
	}
}

func TestCheckUpdate_NewRefCreationAccepted(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db)
	bare, _, c2 := makeBareWithFFChain(t)
	_ = svc.Add(context.Background(), policy.ProtectedRef{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/main",
		BlockDeletion:  true, BlockForcePush: true,
	})
	// Old-OID = nullOID is new ref creation → not a deletion, not a
	// non-FF; accept (Tier 1 doesn't have a block_create toggle).
	if err := svc.CheckUpdate(context.Background(), "acme", "site", bare,
		"refs/heads/main", nullOID, c2); err != nil {
		t.Errorf("CheckUpdate new ref creation: %v, want nil", err)
	}
}

func TestCheckUpdate_GlobMatchesMultiple(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db)
	bare, c1, c2 := makeBareWithFFChain(t)
	_ = svc.Add(context.Background(), policy.ProtectedRef{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/release/*",
		BlockDeletion:  true, BlockForcePush: true,
	})
	err := svc.CheckUpdate(context.Background(), "acme", "site", bare,
		"refs/heads/release/v1", c2, c1)
	var perr *policy.PolicyError
	if !errors.As(err, &perr) {
		t.Errorf("CheckUpdate glob match non-FF: want *PolicyError; got %T %v", err, err)
	}
	if perr != nil && perr.MatchedPattern != "refs/heads/release/*" {
		t.Errorf("MatchedPattern=%q, want refs/heads/release/*", perr.MatchedPattern)
	}
}

func TestCheckUpdate_GlobDoesNotCrossSlash(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db)
	bare, c1, c2 := makeBareWithFFChain(t)
	_ = svc.Add(context.Background(), policy.ProtectedRef{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/*",
		BlockDeletion:  true, BlockForcePush: true,
	})
	// refs/heads/release/v1 does NOT match refs/heads/* (the * doesn't
	// cross the /). Non-FF should succeed.
	if err := svc.CheckUpdate(context.Background(), "acme", "site", bare,
		"refs/heads/release/v1", c2, c1); err != nil {
		t.Errorf("CheckUpdate non-cross-slash: %v, want nil", err)
	}
}

func TestCheckUpdate_AnyBlockingRuleRejects(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := policy.New(db)
	bare, c1, c2 := makeBareWithFFChain(t)
	// Two rules: the specific one allows force-push (both toggles
	// off), the general one blocks it. ANY matching blocking rule
	// must reject.
	_ = svc.Add(context.Background(), policy.ProtectedRef{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/main",
		BlockDeletion:  false, BlockForcePush: false,
	})
	_ = svc.Add(context.Background(), policy.ProtectedRef{
		Tenant: "acme", Repo: "site",
		RefnamePattern: "refs/heads/*",
		BlockDeletion:  true, BlockForcePush: true,
	})
	err := svc.CheckUpdate(context.Background(), "acme", "site", bare,
		"refs/heads/main", c2, c1)
	var perr *policy.PolicyError
	if !errors.As(err, &perr) {
		t.Errorf("CheckUpdate any-rule-blocks: want *PolicyError; got %T %v", err, err)
	}
}
