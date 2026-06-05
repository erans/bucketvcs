package sqlitestore

import (
	"context"
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

func TestGetStorageBinding_NotFound(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()

	_, err := s.GetStorageBinding(ctx, "ghost-tenant")
	if !errors.Is(err, auth.ErrNoSuchBinding) {
		t.Fatalf("want ErrNoSuchBinding, got %v", err)
	}
}

func TestUpsertStorageBinding_RoundTrip(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()

	b := StorageBinding{
		Tenant:     "acme",
		StoreURL:   "s3://my-bucket",
		CredsJSON:  []byte(`{"encrypted": "blob"}`),
		Provider:   "s3compat",
		CreatedAt:  1000,
		UpdatedAt:  1001,
		VerifiedAt: 1002,
	}
	if err := s.UpsertStorageBinding(ctx, b); err != nil {
		t.Fatalf("UpsertStorageBinding: %v", err)
	}

	got, err := s.GetStorageBinding(ctx, "acme")
	if err != nil {
		t.Fatalf("GetStorageBinding: %v", err)
	}
	if got.Tenant != b.Tenant {
		t.Errorf("Tenant = %q, want %q", got.Tenant, b.Tenant)
	}
	if got.StoreURL != b.StoreURL {
		t.Errorf("StoreURL = %q, want %q", got.StoreURL, b.StoreURL)
	}
	if string(got.CredsJSON) != string(b.CredsJSON) {
		t.Errorf("CredsJSON = %q, want %q", got.CredsJSON, b.CredsJSON)
	}
	if got.Provider != b.Provider {
		t.Errorf("Provider = %q, want %q", got.Provider, b.Provider)
	}
	if got.CreatedAt != b.CreatedAt {
		t.Errorf("CreatedAt = %d, want %d", got.CreatedAt, b.CreatedAt)
	}
	if got.UpdatedAt != b.UpdatedAt {
		t.Errorf("UpdatedAt = %d, want %d", got.UpdatedAt, b.UpdatedAt)
	}
	if got.VerifiedAt != b.VerifiedAt {
		t.Errorf("VerifiedAt = %d, want %d", got.VerifiedAt, b.VerifiedAt)
	}
}

func TestUpsertStorageBinding_UpdatesExisting(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()

	first := StorageBinding{
		Tenant:     "acme",
		StoreURL:   "s3://old-bucket",
		CredsJSON:  []byte(`{"v":1}`),
		Provider:   "s3compat",
		CreatedAt:  100,
		UpdatedAt:  100,
		VerifiedAt: 100,
	}
	if err := s.UpsertStorageBinding(ctx, first); err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	second := StorageBinding{
		Tenant:     "acme",
		StoreURL:   "s3://new-bucket",
		CredsJSON:  []byte(`{"v":2}`),
		Provider:   "gcs",
		CreatedAt:  200,
		UpdatedAt:  300,
		VerifiedAt: 400,
	}
	if err := s.UpsertStorageBinding(ctx, second); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	got, err := s.GetStorageBinding(ctx, "acme")
	if err != nil {
		t.Fatalf("GetStorageBinding: %v", err)
	}
	if got.StoreURL != second.StoreURL {
		t.Errorf("StoreURL = %q, want %q (should be updated)", got.StoreURL, second.StoreURL)
	}
	if got.Provider != second.Provider {
		t.Errorf("Provider = %q, want %q", got.Provider, second.Provider)
	}
	if string(got.CredsJSON) != string(second.CredsJSON) {
		t.Errorf("CredsJSON = %q, want %q", got.CredsJSON, second.CredsJSON)
	}
	if got.CreatedAt != first.CreatedAt {
		t.Fatalf("CreatedAt = %d, want %d (must preserve original creation time)", got.CreatedAt, first.CreatedAt)
	}
}

func TestListStorageBindings_ReturnsAll(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()

	for _, tenant := range []string{"beta", "alpha", "gamma"} {
		b := StorageBinding{
			Tenant:     tenant,
			StoreURL:   "s3://" + tenant,
			CredsJSON:  []byte(`{}`),
			Provider:   "s3compat",
			CreatedAt:  1,
			UpdatedAt:  1,
			VerifiedAt: 1,
		}
		if err := s.UpsertStorageBinding(ctx, b); err != nil {
			t.Fatalf("UpsertStorageBinding(%s): %v", tenant, err)
		}
	}

	list, err := s.ListStorageBindings(ctx)
	if err != nil {
		t.Fatalf("ListStorageBindings: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("len = %d, want 3", len(list))
	}
	// Must be ordered by tenant.
	if list[0].Tenant != "alpha" || list[1].Tenant != "beta" || list[2].Tenant != "gamma" {
		t.Errorf("order wrong: %v %v %v", list[0].Tenant, list[1].Tenant, list[2].Tenant)
	}
}

func TestDeleteStorageBinding_RemovesBinding(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()

	b := StorageBinding{
		Tenant:     "acme",
		StoreURL:   "s3://bucket",
		CredsJSON:  []byte(`{}`),
		Provider:   "s3compat",
		CreatedAt:  1,
		UpdatedAt:  1,
		VerifiedAt: 1,
	}
	if err := s.UpsertStorageBinding(ctx, b); err != nil {
		t.Fatalf("UpsertStorageBinding: %v", err)
	}

	if err := s.DeleteStorageBinding(ctx, "acme"); err != nil {
		t.Fatalf("DeleteStorageBinding: %v", err)
	}

	_, err := s.GetStorageBinding(ctx, "acme")
	if !errors.Is(err, auth.ErrNoSuchBinding) {
		t.Fatalf("after delete: want ErrNoSuchBinding, got %v", err)
	}
}

func TestListStorageBindings_EmptyIsNotError(t *testing.T) {
	s := mustOpen(t)
	defer s.Close()
	ctx := context.Background()

	list, err := s.ListStorageBindings(ctx)
	if err != nil {
		t.Fatalf("ListStorageBindings on empty: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected empty slice, got %d rows", len(list))
	}
}
