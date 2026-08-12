package lfs

import (
	"context"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// Wave-4 U-15 regression coverage: before the fix, IssueSSHToken minted an
// unscoped legacy token (auth.ScopeLegacy, empty tenant/repo binding)
// regardless of the git-lfs-authenticate op. The minted token must now bind
// (tenant, repo) with a perm matching the op — download→read, upload→write —
// mirroring OIDC-minted scope binding, and carry the matching LFS scope bit
// so the batch handler's CheckScope enforces it.

// capturingIssuer records the full CreateToken argument vector.
type capturingIssuer struct{ rows []capturingTokenRow }

type capturingTokenRow struct {
	tokenID, userID, secretHash, label string
	expiresAt                          int64
	hasExpires                         bool
	scopes                             auth.TokenScope
	scopeTenant, scopeRepo, scopePerm  string
}

func (c *capturingIssuer) CreateToken(_ context.Context, tokenID, userID, secretHash, label string,
	expiresAt *int64, scopes auth.TokenScope, scopeTenant, scopeRepo, scopePerm string) error {
	row := capturingTokenRow{
		tokenID:     tokenID,
		userID:      userID,
		secretHash:  secretHash,
		label:       label,
		scopes:      scopes,
		scopeTenant: scopeTenant,
		scopeRepo:   scopeRepo,
		scopePerm:   scopePerm,
	}
	if expiresAt != nil {
		row.expiresAt = *expiresAt
		row.hasExpires = true
	}
	c.rows = append(c.rows, row)
	return nil
}

func TestU15_IssueSSHToken_BindsScope_Upload(t *testing.T) {
	iss := &capturingIssuer{}
	_, err := IssueSSHToken(context.Background(), iss, "uid-alice", "alice", "acme", "app",
		"upload", "https://gw.example", 15*time.Minute)
	if err != nil {
		t.Fatalf("IssueSSHToken: %v", err)
	}
	if len(iss.rows) != 1 {
		t.Fatalf("rows=%d, want 1", len(iss.rows))
	}
	row := iss.rows[0]
	if row.scopeTenant != "acme" || row.scopeRepo != "app" {
		t.Errorf("binding = (%q, %q), want (acme, app)", row.scopeTenant, row.scopeRepo)
	}
	if row.scopePerm != "write" {
		t.Errorf("upload scopePerm = %q, want write", row.scopePerm)
	}
	if row.scopes == auth.ScopeLegacy {
		t.Errorf("scopes = legacy(0); want ScopeLFSWrite (upload)")
	}
	if !auth.EffectiveScopes(row.scopes).Has(auth.ScopeLFSWrite) {
		t.Errorf("scopes %v lack lfs:write after EffectiveScopes", row.scopes)
	}
}

func TestU15_IssueSSHToken_BindsScope_Download(t *testing.T) {
	iss := &capturingIssuer{}
	_, err := IssueSSHToken(context.Background(), iss, "uid-alice", "alice", "acme", "app",
		"download", "https://gw.example", 15*time.Minute)
	if err != nil {
		t.Fatalf("IssueSSHToken: %v", err)
	}
	row := iss.rows[0]
	if row.scopePerm != "read" {
		t.Errorf("download scopePerm = %q, want read", row.scopePerm)
	}
	if row.scopeTenant != "acme" || row.scopeRepo != "app" {
		t.Errorf("binding = (%q, %q), want (acme, app)", row.scopeTenant, row.scopeRepo)
	}
	// Download must not carry the write capability.
	if auth.EffectiveScopes(row.scopes).Has(auth.ScopeLFSWrite) {
		t.Errorf("download token scopes %v include lfs:write; want read-only", row.scopes)
	}
	if !auth.EffectiveScopes(row.scopes).Has(auth.ScopeLFSRead) {
		t.Errorf("download token scopes %v lack lfs:read", row.scopes)
	}
}
