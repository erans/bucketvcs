package importer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUploadFileVerified is the deterministic regression test for the
// pack-id-collision flake (see the NOTE in buildcommit_test.go). It proves
// uploadFileVerified is idempotent when the stored bytes match the local file,
// and still fails when a different payload collides on the same key.
func TestUploadFileVerified(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	dir := t.TempDir()

	write := func(name string, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	const key = "tenants/t/repos/r/packs/canonical/deadbeef.pack"

	// First upload to a fresh key succeeds.
	if err := uploadFileVerified(ctx, store, write("a.pack", "PACK-bytes-v1"), key); err != nil {
		t.Fatalf("first upload: %v", err)
	}

	// Re-upload of byte-identical content under the same key is the formerly
	// flaky collision case — it must be an idempotent success.
	if err := uploadFileVerified(ctx, store, write("a-copy.pack", "PACK-bytes-v1"), key); err != nil {
		t.Fatalf("idempotent re-upload of identical bytes should succeed, got: %v", err)
	}

	// A DIFFERENT payload colliding on the same key must error (the corruption
	// guard: a true SHA-1 collision would invalidate our .bvom offsets).
	err := uploadFileVerified(ctx, store, write("b.pack", "PACK-bytes-DIFFERENT"), key)
	if err == nil {
		t.Fatal("upload of differing bytes under an existing key must error")
	}
	if !strings.Contains(err.Error(), "collision") {
		t.Fatalf("error should mention collision, got: %v", err)
	}
}
