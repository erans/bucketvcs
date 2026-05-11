package maintenance_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/maintenance"
	"github.com/bucketvcs/bucketvcs/internal/maintenance/mtest"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

// TestRepack_WritesPackChecksum asserts that after a successful maintenance
// run, every PackEntry written to the manifest carries a 40-hex
// PackChecksum (Git's pack-trailer SHA-1). This is a prerequisite for §16.4
// packfile-uri advertisement (M11 Phase 5).
func TestRepack_WritesPackChecksum(t *testing.T) {
	mtest.GitAvailable(t)
	s := mtest.LocalfsStore(t)
	mtest.SeedRepoFromImport(t, s, "acme", "site")

	ctx := context.Background()
	r, err := repo.Open(ctx, s, "acme", "site")
	if err != nil {
		t.Fatal(err)
	}
	k, err := keys.NewRepo("acme", "site")
	if err != nil {
		t.Fatal(err)
	}

	report, err := maintenance.Run(ctx, s, r, k, maintenance.RunOptions{Force: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Outcome != "success" {
		t.Fatalf("Outcome = %q, want success", report.Outcome)
	}

	view, err := r.ReadRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var body manifest.Body
	if err := json.Unmarshal(view.Body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Packs) == 0 {
		t.Fatalf("post-Run manifest has no packs")
	}
	for i, p := range body.Packs {
		if len(p.PackChecksum) != 40 {
			t.Errorf("body.Packs[%d].PackChecksum = %q (len=%d), want 40-hex",
				i, p.PackChecksum, len(p.PackChecksum))
			continue
		}
		if _, err := hex.DecodeString(p.PackChecksum); err != nil {
			t.Errorf("body.Packs[%d].PackChecksum = %q: not hex: %v",
				i, p.PackChecksum, err)
		}
		// PackID is the trailer SHA-1 too; they should agree on freshly
		// built packs (sanity check, not strictly required by the spec).
		if p.PackID != "" && p.PackChecksum != p.PackID {
			t.Errorf("body.Packs[%d].PackChecksum = %q, PackID = %q; expected equal for fresh packs",
				i, p.PackChecksum, p.PackID)
		}
	}
}
