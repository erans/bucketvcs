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
	"github.com/bucketvcs/bucketvcs/internal/repo/tx"
)

// TestRunPipeline_BackfillsPackChecksum_OnLegacyManifest verifies that when a
// maintenance run encounters a manifest whose PackEntry rows have an empty
// PackChecksum (legacy pre-M11 state), the pipeline backfills the field by
// reading each pack's 20-byte trailer from object storage and CAS-merging
// the updated entries back into the manifest before any other phase runs.
func TestRunPipeline_BackfillsPackChecksum_OnLegacyManifest(t *testing.T) {
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

	// Phase A: produce a "realistic" manifest by running maintenance once
	// with Force. After this, body.Packs has a single repacked pack with
	// PackChecksum populated (per Phase 5.1).
	if _, err := maintenance.Run(ctx, s, r, k, maintenance.RunOptions{Force: true, NoBundle: true}); err != nil {
		t.Fatalf("seed Run (force): %v", err)
	}

	// Phase B: strip PackChecksum from every entry via a CAS commit, so we
	// reproduce a legacy pre-M11 manifest where the importer wrote packs
	// without trailer checksums.
	_, err = r.Commit(ctx, tx.Body{Type: "test_seed_legacy", Actor: "u_test"},
		func(prev *repo.RootView) ([]byte, error) {
			var b manifest.Body
			if uerr := json.Unmarshal(prev.Body, &b); uerr != nil {
				return nil, uerr
			}
			for i := range b.Packs {
				b.Packs[i].PackChecksum = ""
			}
			return manifest.MarshalBody(b)
		})
	if err != nil {
		t.Fatalf("seed legacy commit: %v", err)
	}

	// Verify the legacy state really has empty PackChecksums.
	view, err := r.ReadRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var preBody manifest.Body
	if err := json.Unmarshal(view.Body, &preBody); err != nil {
		t.Fatal(err)
	}
	if len(preBody.Packs) == 0 {
		t.Fatalf("seed manifest has no packs")
	}
	for i, p := range preBody.Packs {
		if p.PackChecksum != "" {
			t.Fatalf("pre-Run body.Packs[%d].PackChecksum = %q; expected empty after strip", i, p.PackChecksum)
		}
	}

	// Phase C: run maintenance again WITHOUT Force and with NoBundle. The
	// pipeline should backfill PackChecksum before any noop short-circuit
	// or further phase runs. Outcome may be "noop" (no triggers fire on a
	// freshly repacked manifest), which is fine — backfill runs before the
	// noop check.
	if _, err := maintenance.Run(ctx, s, r, k, maintenance.RunOptions{NoBundle: true}); err != nil {
		t.Fatalf("Run (backfill): %v", err)
	}

	view, err = r.ReadRoot(ctx)
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
			t.Errorf("Pack %s still missing PackChecksum after backfill: got %q (len=%d)",
				p.PackID, p.PackChecksum, len(p.PackChecksum))
			continue
		}
		if _, err := hex.DecodeString(p.PackChecksum); err != nil {
			t.Errorf("body.Packs[%d].PackChecksum = %q: not hex: %v", i, p.PackChecksum, err)
		}
		// On a pack the repack pipeline produced, PackChecksum should
		// equal PackID (Git uses the trailer SHA-1 as the pack id).
		if p.PackID != "" && p.PackChecksum != p.PackID {
			t.Errorf("body.Packs[%d].PackChecksum=%q, PackID=%q; expected equal", i, p.PackChecksum, p.PackID)
		}
	}
}
