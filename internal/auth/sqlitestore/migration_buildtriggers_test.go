package sqlitestore

import (
	"context"
	"testing"
)

// TestMigration0017_BuildTriggersTablesExist verifies that migration 0017 is
// applied, the _build system user exists, and both new tables accept INSERTs.
// The build_triggers table has a FK to repos(tenant, name), so we seed a repo
// first via RegisterRepo — the same pattern used by oidc_test.go.
func TestMigration0017_BuildTriggersTablesExist(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()

	// _build system user must exist and be enabled.
	var name string
	if err := s.db.QueryRowContext(ctx,
		`SELECT name FROM users WHERE id='_build'`).Scan(&name); err != nil {
		t.Fatalf("expected _build system user: %v", err)
	}
	if name != "_build" {
		t.Fatalf("_build user name = %q, want _build", name)
	}

	// Seed a repo so the FK constraint on build_triggers is satisfied.
	if err := s.RegisterRepo(ctx, "t", "r"); err != nil {
		t.Fatalf("RegisterRepo: %v", err)
	}

	// build_triggers table must accept an INSERT.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO build_triggers
		   (id,tenant,repo,name,kind,config_json,ref_include,ref_exclude,
		    token_mode,token_scopes,token_ttl_seconds,active,created_at)
		 VALUES ('bvbt_x','t','r','n','generic','{}','[]','[]','none',0,900,1,0)`,
	); err != nil {
		t.Fatalf("insert build_triggers: %v", err)
	}

	// build_trigger_deliveries table must accept an INSERT (no FK to build_triggers — orphan-drain design).
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO build_trigger_deliveries
		   (id,trigger_id,payload_json,status,attempts,next_attempt_at,created_at)
		 VALUES ('bvbd_x','bvbt_x','{}','pending',0,0,0)`,
	); err != nil {
		t.Fatalf("insert build_trigger_deliveries: %v", err)
	}
}
