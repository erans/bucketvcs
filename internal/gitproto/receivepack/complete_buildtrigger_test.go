package receivepack

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/buildtrigger"
)

// TestBuildTriggerWiring_NilIsNoOp confirms that when EngineRequest.BuildTriggers
// is nil, the M30 enqueue block in completeReceivePack doesn't fire — the
// pre-M30 default. Mirrors TestPolicyWiring_NilPolicyIsNoOp: we assert the
// field exists with the right type and nil is a valid default (the block is
// guarded by `if eng.BuildTriggers != nil`).
func TestBuildTriggerWiring_NilIsNoOp(t *testing.T) {
	eng := &EngineRequest{}
	if eng.BuildTriggers != nil {
		t.Errorf("default BuildTriggers = %v, want nil", eng.BuildTriggers)
	}
	var _ *buildtrigger.Service = eng.BuildTriggers
}

// TestBuildTriggerWiring_EnqueuesMatchingRef exercises the exact enqueue path
// completeReceivePack runs (Enqueue with a PushInfo built from accepted ref
// updates) against a real buildtrigger.Service backed by a seeded authdb with
// one active trigger on refs/heads/main. It asserts exactly one delivery is
// enqueued, verifying the wiring fires for matching refs. This mirrors the
// M14 policy wiring test convention (drive the dependency directly rather than
// stand up the full multi-dep Service completion).
func TestBuildTriggerWiring_EnqueuesMatchingRef(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	store, err := sqlitestore.Open(filepath.Join(tmp, "auth.db"))
	if err != nil {
		t.Fatalf("sqlitestore.Open: %v", err)
	}
	defer store.Close()
	db := store.DB()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO repos (tenant, name, public_read, created_at)
		 VALUES (?, ?, 0, strftime('%s','now'))`,
		"acme", "site",
	); err != nil {
		t.Fatalf("seed repos: %v", err)
	}

	svc := buildtrigger.New(db)
	if _, err := svc.Create(ctx, buildtrigger.TriggerInput{
		Tenant:     "acme",
		Repo:       "site",
		Name:       "ci",
		Kind:       buildtrigger.KindGeneric,
		Config:     buildtrigger.Config{URL: "https://example.test/hook"},
		RefInclude: []string{"refs/heads/main"},
	}); err != nil {
		t.Fatalf("Create trigger: %v", err)
	}

	// Build the PushInfo exactly as completeReceivePack's M30 block does:
	// accepted ref updates only, HeadOID via buildTriggerHeadOID.
	refUpdates := []buildtrigger.RefUpdate{
		{
			Refname: "refs/heads/main",
			OldOID:  "0000000000000000000000000000000000000000",
			NewOID:  "1111111111111111111111111111111111111111",
		},
		// A non-matching ref must NOT produce a delivery.
		{
			Refname: "refs/heads/dev",
			OldOID:  "0000000000000000000000000000000000000000",
			NewOID:  "2222222222222222222222222222222222222222",
		},
	}
	push := buildtrigger.PushInfo{
		Tenant:     "acme",
		Repo:       "site",
		Actor:      "pusher",
		TxID:       "tx-1",
		HeadOID:    buildTriggerHeadOID(refUpdates),
		RefUpdates: refUpdates,
	}
	if push.HeadOID != "1111111111111111111111111111111111111111" {
		t.Fatalf("buildTriggerHeadOID = %q, want first non-null NewOID", push.HeadOID)
	}

	if err := svc.Enqueue(ctx, push); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	pending, err := svc.PendingForTest(ctx)
	if err != nil {
		t.Fatalf("PendingForTest: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending deliveries = %d, want 1 (only refs/heads/main matches)", len(pending))
	}
	if pending[0].Status != "pending" {
		t.Errorf("delivery status = %q, want pending", pending[0].Status)
	}
}

// TestBuildTriggerHeadOID covers the helper's null-OID handling: delete-only
// pushes (every NewOID is the null OID) return "".
func TestBuildTriggerHeadOID(t *testing.T) {
	null := "0000000000000000000000000000000000000000"
	cases := []struct {
		name string
		in   []buildtrigger.RefUpdate
		want string
	}{
		{"empty", nil, ""},
		{"delete-only", []buildtrigger.RefUpdate{{Refname: "refs/heads/main", NewOID: null}}, ""},
		{"first-non-null", []buildtrigger.RefUpdate{
			{Refname: "refs/heads/del", NewOID: null},
			{Refname: "refs/heads/main", NewOID: "abc123"},
		}, "abc123"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildTriggerHeadOID(tc.in); got != tc.want {
				t.Errorf("buildTriggerHeadOID = %q, want %q", got, tc.want)
			}
		})
	}
}
