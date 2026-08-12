package sqlitestore

import (
	"context"
	"strings"
	"testing"
)

func TestRenameRepo_RejectsInvalidDestinationButCanRepairLegacySource_U9Review(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	if err := s.RegisterRepo(ctx, "acme", "source"); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"my.repo", "..", strings.Repeat("r", 129)} {
		if err := s.RenameRepo(ctx, "acme", "source", invalid); err == nil {
			t.Errorf("RenameRepo to %q succeeded", invalid)
		}
	}

	// Seed a pre-fix legacy row directly: validation must not reject oldName,
	// otherwise operators cannot repair historical unusable registrations.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO repos (tenant, name, public_read, created_at) VALUES (?, ?, 0, 1)`,
		"acme", "legacy.repo"); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameRepo(ctx, "acme", "legacy.repo", "repaired"); err != nil {
		t.Fatalf("repair legacy source: %v", err)
	}
}

func TestRegisterRepo_RejectsInvalidDurableKeyNames_U9(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	for _, tc := range []struct{ tenant, repo string }{
		{"acme", "my.repo"}, {".hidden", "repo"}, {"acme", ".."},
		{strings.Repeat("t", 129), "repo"}, {"acme", strings.Repeat("r", 129)},
	} {
		if err := s.RegisterRepo(ctx, tc.tenant, tc.repo); err == nil {
			t.Errorf("RegisterRepo(%q, %q) succeeded", tc.tenant, tc.repo)
		}
	}
	if err := s.RegisterRepo(ctx, "acme", "repo-1"); err != nil {
		t.Fatalf("valid registration failed: %v", err)
	}
}
