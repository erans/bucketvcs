package webhooks_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

// openTestDB returns a fresh on-disk authdb with migrations applied,
// pre-seeded with a (tenant, repo) row so the FK on webhook_endpoints
// is satisfiable. Mirrors the M13.5/M14 test shape.
func openTestDB(t *testing.T, tenant, repo string) *sql.DB {
	t.Helper()
	tmp := t.TempDir()
	store, err := sqlitestore.Open(filepath.Join(tmp, "auth.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db := store.DB()
	if _, err := db.Exec(
		`INSERT INTO repos (tenant, name, public_read, created_at)
		 VALUES (?, ?, 0, strftime('%s','now'))`,
		tenant, repo,
	); err != nil {
		t.Fatalf("seed repo row: %v", err)
	}
	return db
}

func TestService_CreateListRemove(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := webhooks.New(db)
	ctx := context.Background()

	got, err := svc.List(ctx, "acme", "site")
	if err != nil {
		t.Fatalf("List empty: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("List empty: len=%d, want 0", len(got))
	}

	ep, err := svc.Create(ctx, webhooks.EndpointInput{
		Tenant:    "acme",
		Repo:      "site",
		URL:       "https://hooks.example.com/bucketvcs",
		EventMask: webhooks.EventPush | webhooks.EventLFSUpload,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if ep.ID == 0 {
		t.Errorf("Create returned ID=0; want assigned")
	}
	if ep.Secret == "" || len(ep.Secret) < 32 {
		t.Errorf("Create returned secret=%q; want >=32 char random secret", ep.Secret)
	}
	if !ep.Active {
		t.Errorf("Create returned Active=false; want true (default)")
	}

	got, err = svc.List(ctx, "acme", "site")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].ID != ep.ID {
		t.Fatalf("List: got %+v, want one row with ID=%d", got, ep.ID)
	}
	if got[0].Secret != "" {
		t.Errorf("List returned full secret; want redacted")
	}
	if got[0].SecretPreview == "" {
		t.Errorf("List returned empty SecretPreview")
	}

	gotOne, err := svc.Get(ctx, ep.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if gotOne.ID != ep.ID {
		t.Errorf("Get ID=%d, want %d", gotOne.ID, ep.ID)
	}
	if gotOne.Secret != "" {
		t.Errorf("Get returned full secret; want redacted")
	}

	if err := svc.Remove(ctx, ep.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	got, _ = svc.List(ctx, "acme", "site")
	if len(got) != 0 {
		t.Errorf("after Remove: len=%d, want 0", len(got))
	}
}

func TestService_CreateRejectsDuplicateURL(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := webhooks.New(db)
	in := webhooks.EndpointInput{
		Tenant:    "acme",
		Repo:      "site",
		URL:       "https://hooks.example.com/bucketvcs",
		EventMask: webhooks.EventPush,
	}
	if _, err := svc.Create(context.Background(), in); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	_, err := svc.Create(context.Background(), in)
	if err == nil {
		t.Errorf("second Create with same URL returned nil; want unique violation")
	}
	if !errors.Is(err, webhooks.ErrConflict) {
		t.Errorf("second Create: err=%v, want ErrConflict", err)
	}
}

func TestService_GetNotFound(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := webhooks.New(db)
	_, err := svc.Get(context.Background(), 99999)
	if !errors.Is(err, webhooks.ErrNotFound) {
		t.Errorf("Get(non-existent): err=%v, want ErrNotFound", err)
	}
}

func TestService_RemoveAndDisableNotFound(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := webhooks.New(db)
	if err := svc.Remove(context.Background(), 99999); !errors.Is(err, webhooks.ErrNotFound) {
		t.Errorf("Remove(non-existent): err=%v, want ErrNotFound", err)
	}
	if err := svc.Disable(context.Background(), 99999); !errors.Is(err, webhooks.ErrNotFound) {
		t.Errorf("Disable(non-existent): err=%v, want ErrNotFound", err)
	}
}

func TestService_CreateRejectsBadInput(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := webhooks.New(db)
	cases := []struct {
		name string
		in   webhooks.EndpointInput
	}{
		{"empty tenant", webhooks.EndpointInput{Repo: "site", URL: "https://x", EventMask: webhooks.EventPush}},
		{"empty repo", webhooks.EndpointInput{Tenant: "acme", URL: "https://x", EventMask: webhooks.EventPush}},
		{"empty URL", webhooks.EndpointInput{Tenant: "acme", Repo: "site", EventMask: webhooks.EventPush}},
		{"bad URL scheme", webhooks.EndpointInput{Tenant: "acme", Repo: "site", URL: "ftp://x", EventMask: webhooks.EventPush}},
		{"zero event mask", webhooks.EndpointInput{Tenant: "acme", Repo: "site", URL: "https://x"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := svc.Create(context.Background(), c.in); err == nil {
				t.Errorf("Create %s returned nil; want error", c.name)
			}
		})
	}
}

func TestService_EnableDisable(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := webhooks.New(db)
	ctx := context.Background()
	ep, err := svc.Create(ctx, webhooks.EndpointInput{
		Tenant: "acme", Repo: "site",
		URL:       "https://hooks.example.com/bucketvcs",
		EventMask: webhooks.EventPush,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Disable(ctx, ep.ID); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	got, _ := svc.Get(ctx, ep.ID)
	if got.Active {
		t.Errorf("after Disable: Active=true, want false")
	}
	if err := svc.Enable(ctx, ep.ID); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	got, _ = svc.Get(ctx, ep.ID)
	if !got.Active {
		t.Errorf("after Enable: Active=false, want true")
	}
}

func TestService_RotateSecret(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := webhooks.New(db)
	ctx := context.Background()

	ep, err := svc.Create(ctx, webhooks.EndpointInput{
		Tenant: "acme", Repo: "site",
		URL:       "https://hooks.example.com/x",
		EventMask: webhooks.EventPush,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	oldSecret := ep.Secret

	newSecret, err := svc.RotateSecret(ctx, ep.ID)
	if err != nil {
		t.Fatalf("RotateSecret: %v", err)
	}
	if newSecret == "" {
		t.Errorf("RotateSecret returned empty new secret")
	}
	if newSecret == oldSecret {
		t.Errorf("RotateSecret returned the same secret: %q", newSecret)
	}
	if len(newSecret) < 32 {
		t.Errorf("RotateSecret returned short secret: %d bytes", len(newSecret))
	}

	got, err := svc.GetWithSecret(ctx, ep.ID)
	if err != nil {
		t.Fatalf("GetWithSecret: %v", err)
	}
	if got.Secret != newSecret {
		t.Errorf("GetWithSecret returned %q, want %q", got.Secret, newSecret)
	}
}

func TestService_RotateSecretNotFound(t *testing.T) {
	db := openTestDB(t, "acme", "site")
	svc := webhooks.New(db)
	_, err := svc.RotateSecret(context.Background(), 99999)
	if !errors.Is(err, webhooks.ErrNotFound) {
		t.Errorf("RotateSecret(non-existent): err=%v, want ErrNotFound", err)
	}
}
