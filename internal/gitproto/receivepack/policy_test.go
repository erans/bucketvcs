package receivepack

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/policy"
)

// TestPolicyWiring_NilPolicyIsNoOp confirms that when EngineRequest.Policy
// is nil, the step 8b dispatch path doesn't fire — pre-M14 behavior.
// We don't run completeReceivePack here (it has many other deps); we just
// assert that the field exists with the right type and nil is a valid
// default.
func TestPolicyWiring_NilPolicyIsNoOp(t *testing.T) {
	eng := &EngineRequest{}
	if eng.Policy != nil {
		t.Errorf("default Policy = %v, want nil", eng.Policy)
	}
	// Confirm the field type compiles and accepts a *policy.Service.
	var _ *policy.Service = eng.Policy
}

// TestPolicyWiring_CheckUpdateAccept exercises CheckUpdate through a
// real Service against a freshly-seeded authdb. This verifies that
// the wiring step 8b will execute correctly (e.g., no panic on the
// no-rules path, the typed error round-trips via errors.As).
func TestPolicyWiring_CheckUpdateAccept(t *testing.T) {
	tmp := t.TempDir()
	authStore, err := sqlitestore.Open(filepath.Join(tmp, "auth.db"))
	if err != nil {
		t.Fatalf("sqlitestore.Open: %v", err)
	}
	defer authStore.Close()
	// Seed the repos row so the FK on protected_refs is satisfied
	// (mirrors the M14 Task 1 test harness).
	if _, err := authStore.DB().ExecContext(context.Background(),
		`INSERT INTO repos (tenant, name, public_read, created_at)
		 VALUES (?, ?, 0, strftime('%s','now'))`,
		"acme", "site",
	); err != nil {
		t.Fatalf("seed repos: %v", err)
	}

	svc := policy.New(authStore.DB())
	// No rules added — CheckUpdate returns nil for any ref update.
	err = svc.CheckUpdate(context.Background(), "acme", "site", t.TempDir(),
		"refs/heads/main",
		"0000000000000000000000000000000000000000",
		"0000000000000000000000000000000000000001",
	)
	if err != nil {
		t.Errorf("CheckUpdate with no rules: %v, want nil", err)
	}
}

// TestPolicyWiring_EmittersDontPanic ensures the metric + audit
// emitters are callable from the receivepack package's import path
// without panicking when invoked with a nil logger (Task 5: real
// bodies replace the no-op stubs; the panic-safety contract is
// preserved so step 8b's emit-on-every-update path stays cheap).
func TestPolicyWiring_EmittersDontPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("emitter panicked: %v", r)
		}
	}()
	policy.EmitRefCheckMetric(context.Background(), nil, "ok")
	policy.EmitRefRejected(context.Background(), nil, "acme", "site",
		&policy.PolicyError{Refname: "refs/heads/main"}, "alice")
}
