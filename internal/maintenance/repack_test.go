package maintenance

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/maintenance/mtest"
)

func TestRepack_ProducesSinglePackPair(t *testing.T) {
	// SetupSyntheticBareRepo returns a bare repo path. Repack expects
	// to find <bareDir>/bare.git/objects/, so we wrap it: copy/move the
	// bare repo under a parent dir as "bare.git".
	srcBare := mtest.SetupSyntheticBareRepo(t)
	parent := t.TempDir()
	dst := filepath.Join(parent, "bare.git")
	if err := os.Rename(srcBare, dst); err != nil {
		t.Fatalf("rename bare into parent: %v", err)
	}

	out, err := Repack(context.Background(), parent)
	if err != nil {
		t.Fatalf("Repack: %v", err)
	}
	if out.PackID == "" {
		t.Fatal("PackID empty")
	}
	if _, err := os.Stat(out.PackPath); err != nil {
		t.Fatalf("pack file missing: %v", err)
	}
	if _, err := os.Stat(out.IdxPath); err != nil {
		t.Fatalf("idx file missing: %v", err)
	}
	if out.SizeBytes <= 0 {
		t.Fatalf("SizeBytes = %d, want > 0", out.SizeBytes)
	}
	if filepath.Dir(out.PackPath) != filepath.Dir(out.IdxPath) {
		t.Errorf("pack and idx in different dirs: %s vs %s", out.PackPath, out.IdxPath)
	}
}
