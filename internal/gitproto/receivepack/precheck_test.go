package receivepack

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest/manifesttest"
	"github.com/bucketvcs/bucketvcs/internal/repo/refstore"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// TestPrecheckUpdates_ShardedBody verifies that precheckUpdates correctly
// validates old-OID preconditions against a sharded (v2) manifest body.
// Uses manifesttest.MakeShardedBody to avoid inline-body assumptions.
func TestPrecheckUpdates_ShardedBody(t *testing.T) {
	ctx := context.Background()

	store, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	defer store.Close()

	k, err := keys.NewRepo("acme", "precheck-test")
	if err != nil {
		t.Fatalf("keys.NewRepo: %v", err)
	}

	const (
		oidX = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		oidY = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		oidZ = "cccccccccccccccccccccccccccccccccccccccc"
	)

	existingRefs := map[string]string{
		"refs/heads/main": oidX,
	}
	body, err := manifesttest.MakeShardedBody(ctx, store, k, "refs/heads/main", existingRefs)
	if err != nil {
		t.Fatalf("MakeShardedBody: %v", err)
	}

	rs, err := refstore.New(ctx, store, k, &body)
	if err != nil {
		t.Fatalf("refstore.New: %v", err)
	}

	// Valid update: OldOID=X (matches current), NewOID=Y.
	t.Run("valid update passes precheck", func(t *testing.T) {
		updates := []updateCommand{
			{Refname: "refs/heads/main", OldOID: oidX, NewOID: oidY},
		}
		statuses, allOK := precheckUpdates(ctx, rs, updates)
		if !allOK {
			t.Errorf("expected allOK=true, got false; statuses=%v", statuses)
		}
		if statuses[0] != "" {
			t.Errorf("expected empty status for valid update, got %q", statuses[0])
		}
	})

	// Stale update: OldOID=Z (wrong), NewOID=Y — must fail with "stale info".
	t.Run("stale OldOID fails with stale info", func(t *testing.T) {
		updates := []updateCommand{
			{Refname: "refs/heads/main", OldOID: oidZ, NewOID: oidY},
		}
		statuses, allOK := precheckUpdates(ctx, rs, updates)
		if allOK {
			t.Error("expected allOK=false for stale update")
		}
		if !strings.Contains(statuses[0], "stale info") {
			t.Errorf("expected 'stale info' in status, got %q", statuses[0])
		}
	})

	// Create update: OldOID=nullOID for existing ref — must fail with "ref already exists".
	t.Run("create on existing ref fails", func(t *testing.T) {
		updates := []updateCommand{
			{Refname: "refs/heads/main", OldOID: nullOID, NewOID: oidY},
		}
		statuses, allOK := precheckUpdates(ctx, rs, updates)
		if allOK {
			t.Error("expected allOK=false for create on existing ref")
		}
		if !strings.Contains(statuses[0], "ref already exists") {
			t.Errorf("expected 'ref already exists' in status, got %q", statuses[0])
		}
	})

	// Create update: OldOID=nullOID for non-existing ref — must pass.
	t.Run("create on new ref passes", func(t *testing.T) {
		updates := []updateCommand{
			{Refname: "refs/heads/new-branch", OldOID: nullOID, NewOID: oidY},
		}
		statuses, allOK := precheckUpdates(ctx, rs, updates)
		if !allOK {
			t.Errorf("expected allOK=true for new ref creation, got false; statuses=%v", statuses)
		}
		if statuses[0] != "" {
			t.Errorf("expected empty status for new ref creation, got %q", statuses[0])
		}
	})
}

// listFailingRefStore is a RefStore whose List always returns an error.
// Used to verify that precheckUpdates marks every update as "backend-error"
// when the underlying store cannot enumerate refs.
type listFailingRefStore struct{}

func (listFailingRefStore) Mode() refstore.Mode { return refstore.ModeInline }
func (listFailingRefStore) Lookup(_ context.Context, _ string) (string, bool, error) {
	return "", false, nil
}
func (listFailingRefStore) List(_ context.Context) (map[string]string, error) {
	return nil, errors.New("forced list failure")
}
func (listFailingRefStore) Stage(_ context.Context, _ map[string]string) (refstore.Stage, error) {
	return refstore.Stage{}, errors.New("not used")
}

func TestPrecheckUpdates_ListFailureFailsAllUpdates(t *testing.T) {
	updates := []updateCommand{
		{Refname: "refs/heads/main", OldOID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", NewOID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		{Refname: "refs/heads/dev", OldOID: "cccccccccccccccccccccccccccccccccccccccc", NewOID: "dddddddddddddddddddddddddddddddddddddddd"},
	}
	statuses, allOK := precheckUpdates(context.Background(), listFailingRefStore{}, updates)
	if allOK {
		t.Errorf("allOK=true, want false on backend error")
	}
	if len(statuses) != len(updates) {
		t.Fatalf("statuses len=%d want %d", len(statuses), len(updates))
	}
	for i, s := range statuses {
		if !strings.Contains(s, "backend-error") {
			t.Errorf("statuses[%d]=%q want contains backend-error", i, s)
		}
	}
}
