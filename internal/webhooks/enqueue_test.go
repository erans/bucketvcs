package webhooks_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

func TestEnqueue_OneRowPerMatchingEndpoint(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := webhooks.New(db)
	ctx := context.Background()

	pushEP, err := svc.Create(ctx, webhooks.EndpointInput{
		Tenant: "acme", Repo: "site",
		URL:       "https://push-only",
		EventMask: webhooks.EventPush,
	})
	if err != nil {
		t.Fatalf("Create push EP: %v", err)
	}
	repoEP, err := svc.Create(ctx, webhooks.EndpointInput{
		Tenant: "acme", Repo: "site",
		URL:       "https://repo-only",
		EventMask: webhooks.EventRepoCreated,
	})
	if err != nil {
		t.Fatalf("Create repo EP: %v", err)
	}

	if err := svc.Enqueue(ctx, webhooks.EventPush, "acme", "site", "alice",
		webhooks.PushPayload{TxID: "tx-1", ManifestVersion: 42, StorageBackend: "localfs"},
	); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	rows, err := svc.PendingForTest(ctx)
	if err != nil {
		t.Fatalf("PendingForTest: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("after push Enqueue: rows=%d, want 1", len(rows))
	}
	if rows[0].EndpointID != pushEP.ID {
		t.Errorf("row.EndpointID=%d, want %d (pushEP)", rows[0].EndpointID, pushEP.ID)
	}
	if rows[0].EventType != "push" {
		t.Errorf("row.EventType=%q, want \"push\"", rows[0].EventType)
	}

	var generic map[string]any
	if err := json.Unmarshal(rows[0].PayloadJSON, &generic); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if generic["tenant"] != "acme" || generic["repo"] != "site" {
		t.Errorf("payload missing envelope fields: %+v", generic)
	}
	if generic["actor"] != "alice" {
		t.Errorf("payload actor=%v, want alice", generic["actor"])
	}
	if generic["delivery_id"] == "" || generic["delivery_id"] == nil {
		t.Errorf("payload delivery_id missing")
	}

	if err := svc.Enqueue(ctx, webhooks.EventRepoCreated, "acme", "site", "alice",
		webhooks.RepoLifecyclePayload{},
	); err != nil {
		t.Fatalf("Enqueue repo: %v", err)
	}
	rows, _ = svc.PendingForTest(ctx)
	if len(rows) != 2 {
		t.Fatalf("after repo.created Enqueue: rows=%d, want 2", len(rows))
	}

	if err := svc.Disable(ctx, repoEP.ID); err != nil {
		t.Fatalf("Disable repoEP: %v", err)
	}
	if err := svc.Enqueue(ctx, webhooks.EventRepoCreated, "acme", "site", "alice",
		webhooks.RepoLifecyclePayload{},
	); err != nil {
		t.Fatalf("Enqueue after Disable: %v", err)
	}
	rows, _ = svc.PendingForTest(ctx)
	if len(rows) != 2 {
		t.Errorf("after Disable + Enqueue: rows=%d, want still 2", len(rows))
	}
}

func TestEnqueue_NilServiceIsNoOp(t *testing.T) {
	var svc *webhooks.Service
	if err := svc.Enqueue(context.Background(), webhooks.EventPush, "acme", "site", "alice",
		webhooks.PushPayload{},
	); err != nil {
		t.Errorf("nil Service.Enqueue returned %v, want nil", err)
	}
}

func TestEnqueue_OtherRepoNotNotified(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := webhooks.New(db)
	ctx := context.Background()
	if _, err := svc.Create(ctx, webhooks.EndpointInput{
		Tenant: "acme", Repo: "site",
		URL: "https://site-hook", EventMask: webhooks.EventPush,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Enqueue(ctx, webhooks.EventPush, "acme", "other", "alice",
		webhooks.PushPayload{},
	); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	rows, _ := svc.PendingForTest(ctx)
	if len(rows) != 0 {
		t.Errorf("enqueue for other repo produced %d rows; want 0", len(rows))
	}
}
