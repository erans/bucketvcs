package gc

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/gc/marks"
	"github.com/bucketvcs/bucketvcs/internal/gc/sweeps"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/oklog/ulid/v2"
)

// SweepOptions configures one RunSweep invocation.
type SweepOptions struct {
	// Now is a clock injection point for tests; defaults to time.Now.
	Now func() time.Time
	// MaxConcurrency is reserved for future parallel-sweep implementation;
	// the current implementation processes candidates sequentially.
	// Zero or negative is normalized to 1.
	MaxConcurrency int
	// DryRun, when true, classifies candidates normally but skips all
	// Head and DeleteIfVersionMatches calls. Candidates that would be
	// deleted are still appended to out.Deleted.* so the caller can
	// report "would delete" counts without modifying storage.
	DryRun bool
}

// RunSweep executes the sweep phase against the given mark record.
// Returns the (unwritten) sweep Record. Caller writes via sweeps.Write.
//
// Individual delete failures are accumulated in Record.Errors, not
// returned as an error; the returned error covers only fatal setup
// failures (key derivation, root-read).
func RunSweep(ctx context.Context, s storage.ObjectStore, r *repo.Repo, mark marks.Record, opts SweepOptions) (sweeps.Record, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.MaxConcurrency < 1 {
		opts.MaxConcurrency = 1
	}

	k, err := keys.NewRepo(r.TenantID(), r.RepoID())
	if err != nil {
		return sweeps.Record{}, fmt.Errorf("gc: keys: %w", err)
	}

	startedAt := opts.Now().UTC()
	view, err := r.ReadRoot(ctx)
	if err != nil {
		return sweeps.Record{}, fmt.Errorf("gc: read root: %w", err)
	}
	freshLive, err := BuildLiveSet(k, view.Header, view.Body)
	if err != nil {
		return sweeps.Record{}, fmt.Errorf("gc: build live set: %w", err)
	}
	retention := time.Duration(mark.RetentionSeconds) * time.Second

	out := sweeps.Record{
		SchemaVersion:                sweeps.SchemaVersion,
		SweepID:                      newSweepID(startedAt),
		MarkID:                       mark.MarkID,
		StartedAt:                    startedAt,
		CurrentManifestVersion:       view.Header.ManifestVersion,
		CurrentManifestObjectVersion: view.Version.Token,
	}

	now := startedAt

	// Categories are processed sequentially; within each category,
	// candidates are also processed sequentially. MaxConcurrency is
	// normalized for forward-compat (an opts.MaxConcurrency=0 still
	// produces a valid SweepOptions) but is not yet plumbed into a
	// worker pool — revisit when sweep throughput becomes a bottleneck.
	for _, t := range mark.Candidates.TxRecords {
		decision := classify(t.Key, "tx_records", t.FirstSeenUnreachableAt, retention, now, freshLive, mark.TxOrphanSweepArmed)
		applyDecision(ctx, s, t.Key, "tx_records", decision, opts.DryRun, &out)
	}
	for _, p := range mark.Candidates.CanonicalPacks {
		decision := classify(p.Key, "canonical_packs", p.FirstSeenUnreachableAt, retention, now, freshLive, true /* armed N/A */)
		applyDecision(ctx, s, p.Key, "canonical_packs", decision, opts.DryRun, &out)
	}
	for _, i := range mark.Candidates.Indexes {
		decision := classify(i.Key, "indexes", i.FirstSeenUnreachableAt, retention, now, freshLive, true /* armed N/A */)
		applyDecision(ctx, s, i.Key, "indexes", decision, opts.DryRun, &out)
	}

	out.CompletedAt = opts.Now().UTC()
	return out, nil
}

// decision is the outcome of pre-flight classification per candidate.
type decision int

const (
	decideDelete decision = iota
	decideRevived
	decideRetention
	decideDisarmed
)

func classify(key, category string, firstSeen time.Time, retention time.Duration, now time.Time, live LiveSet, armed bool) decision {
	if _, ok := live[key]; ok {
		return decideRevived
	}
	if now.Sub(firstSeen) < retention {
		return decideRetention
	}
	if category == "tx_records" && !armed {
		return decideDisarmed
	}
	return decideDelete
}

func applyDecision(ctx context.Context, s storage.ObjectStore, key, category string, d decision, dryRun bool, out *sweeps.Record) {
	switch d {
	case decideRevived:
		out.Skipped = append(out.Skipped, sweeps.SkippedEntry{Key: key, Category: category, Reason: "revived"})
		return
	case decideRetention:
		out.Skipped = append(out.Skipped, sweeps.SkippedEntry{Key: key, Category: category, Reason: "retention_not_met"})
		return
	case decideDisarmed:
		out.Skipped = append(out.Skipped, sweeps.SkippedEntry{Key: key, Category: category, Reason: "tx_sweep_disarmed"})
		return
	}

	// decideDelete path.
	if dryRun {
		// Compute candidates but skip all storage mutations. Append to
		// Deleted so the caller can report "would delete" counts.
		switch category {
		case "tx_records":
			out.Deleted.TxRecords = append(out.Deleted.TxRecords, key)
		case "canonical_packs":
			out.Deleted.CanonicalPacks = append(out.Deleted.CanonicalPacks, key)
		case "indexes":
			out.Deleted.Indexes = append(out.Deleted.Indexes, key)
		}
		return
	}

	meta, err := s.Head(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			out.Skipped = append(out.Skipped, sweeps.SkippedEntry{Key: key, Category: category, Reason: "not_found"})
			return
		}
		out.Errors = append(out.Errors, sweeps.ErrorEntry{Key: key, Category: category, Error: err.Error()})
		return
	}
	if err := s.DeleteIfVersionMatches(ctx, key, meta.Version); err != nil {
		switch {
		case errors.Is(err, storage.ErrVersionMismatch):
			out.Skipped = append(out.Skipped, sweeps.SkippedEntry{Key: key, Category: category, Reason: "version_mismatch"})
		case errors.Is(err, storage.ErrNotFound):
			out.Skipped = append(out.Skipped, sweeps.SkippedEntry{Key: key, Category: category, Reason: "not_found"})
		default:
			out.Errors = append(out.Errors, sweeps.ErrorEntry{Key: key, Category: category, Error: err.Error()})
		}
		return
	}

	switch category {
	case "tx_records":
		out.Deleted.TxRecords = append(out.Deleted.TxRecords, key)
		// Note: by classification, an orphan tx record has no .commit
		// sibling. If a future repair tool injects markers, it must
		// clean its own markers — this code path does not.
	case "canonical_packs":
		out.Deleted.CanonicalPacks = append(out.Deleted.CanonicalPacks, key)
	case "indexes":
		out.Deleted.Indexes = append(out.Deleted.Indexes, key)
	}
}

var sweepEntropy = &ulid.LockedMonotonicReader{
	MonotonicReader: ulid.Monotonic(rand.New(rand.NewSource(time.Now().UnixNano())), 0),
}

func newSweepID(at time.Time) string {
	return "sw_" + ulid.MustNew(ulid.Timestamp(at), sweepEntropy).String()
}
