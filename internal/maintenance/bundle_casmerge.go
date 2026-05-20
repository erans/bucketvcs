package maintenance

import (
	"context"
	"fmt"

	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/tx"
)

// MergeBundleEntry replaces the existing full_default bundle in body
// with fresh, leaving every other field untouched. Other bundle kinds
// (none in M11; rolling-base / release-tag in successors) are preserved.
//
// Pure function — safe to call inside a CAS retry callback. Returns an
// error if fresh.Kind != "full_default": M11's contract is that only
// the full_default variant is ever written by this path.
func MergeBundleEntry(body manifest.Body, fresh manifest.BundleEntry) (manifest.Body, error) {
	if fresh.Kind != "full_default" {
		return body, fmt.Errorf("bundle merge: M11 only writes Kind=full_default, got %q", fresh.Kind)
	}
	var kept []manifest.BundleEntry
	for _, e := range body.Bundles {
		if e.Kind == "full_default" {
			continue
		}
		kept = append(kept, e)
	}
	body.Bundles = append(kept, fresh)
	return body, nil
}

// RunBundleCASMerge CAS-merges fresh into the repo's root manifest.
// Re-reads the manifest under repo.Repo.Commit's retry loop and applies
// MergeBundleEntry per attempt, so concurrent pushes / maintenance runs
// that touched other manifest fields are preserved on conflict.
//
// actor is the tx.Body Actor string ("u_op" in production maintenance
// runs). casRetry bounds the CAS attempts (defaults to maintenance's
// DefaultCASRetry when non-positive). Returns the error from Commit
// verbatim — callers should errors.As against *repoerrs.CommitGaveUpError
// to distinguish retry exhaustion from other failures.
func RunBundleCASMerge(ctx context.Context, r *repo.Repo, fresh manifest.BundleEntry, actor string, casRetry int) error {
	if casRetry <= 0 {
		casRetry = DefaultCASRetry
	}
	if actor == "" {
		actor = "u_op"
	}
	txBody := tx.Body{Type: "maintenance_bundle", Actor: actor}
	_, err := r.Commit(ctx, txBody, func(prev *repo.RootView) ([]byte, error) {
		prevBody, perr := manifest.UnmarshalBody(prev.Body)
		if perr != nil {
			return nil, fmt.Errorf("bundle cas: unmarshal: %w", perr)
		}
		merged, mErr := MergeBundleEntry(prevBody, fresh)
		if mErr != nil {
			return nil, mErr
		}
		return manifest.MarshalBody(merged)
	}, repo.WithCommitPolicy(repo.CommitPolicy{MaxRetries: casRetry}))
	return err
}
