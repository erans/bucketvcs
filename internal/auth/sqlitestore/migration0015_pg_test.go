//go:build postgres

package sqlitestore

import (
	"testing"
)

// TestPGMigration0015DropsWebhookEndpointsFK asserts no FK from
// webhook_endpoints to repos survives migration 0015.
func TestPGMigration0015DropsWebhookEndpointsFK(t *testing.T) {
	s := openPostgres(t)
	var n int
	if err := s.db.QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM pg_constraint
		 WHERE conrelid = 'webhook_endpoints'::regclass
		   AND contype = 'f'
		   AND confrelid = 'repos'::regclass`).Scan(&n); err != nil {
		t.Fatalf("pg_constraint query: %v", err)
	}
	if n != 0 {
		t.Fatalf("webhook_endpoints still has %d FK(s) to repos; migration 0015 did not drop it", n)
	}
}
