package gc

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/gc/marks"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/oklog/ulid/v2"
)

// MarkOptions configures one RunMark invocation.
type MarkOptions struct {
	// Now is a clock injection point for tests; defaults to time.Now.
	Now func() time.Time
	// RetentionSeconds is the retention window pinned into the mark
	// record. Sweep honors this value, not the operator's current flag.
	RetentionSeconds int
}

// RunMark executes the mark phase for one repo and returns the (unwritten)
// mark Record. Caller writes the record via marks.Write.
func RunMark(ctx context.Context, s storage.ObjectStore, r *repo.Repo, opts MarkOptions) (marks.Record, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.RetentionSeconds <= 0 {
		opts.RetentionSeconds = int(DefaultRetention.Seconds())
	}

	k, err := keys.NewRepo(r.TenantID(), r.RepoID())
	if err != nil {
		return marks.Record{}, fmt.Errorf("gc: keys: %w", err)
	}
	view, err := r.ReadRoot(ctx)
	if err != nil {
		return marks.Record{}, fmt.Errorf("gc: read root: %w", err)
	}
	live := BuildLiveSet(k, view.Header, view.Body)

	startedAt := opts.Now().UTC()
	markID := newMarkID(startedAt)

	prev, err := marks.ReadLatest(ctx, s, k)
	hasPrev := err == nil
	if err != nil && !errors.Is(err, marks.ErrNotFound) {
		return marks.Record{}, fmt.Errorf("gc: read previous mark: %w", err)
	}

	packCandKeys, err := DiscoverCanonicalPacks(ctx, s, k, live)
	if err != nil {
		return marks.Record{}, err
	}
	indexCandKeys, err := DiscoverIndexes(ctx, s, k, live)
	if err != nil {
		return marks.Record{}, err
	}
	txCandKeys, armedNow, err := DiscoverTxRecords(ctx, s, k, live)
	if err != nil {
		return marks.Record{}, err
	}

	// Sticky-armed: once any prior run observed a marker, stay armed forever.
	armed := armedNow
	if hasPrev && prev.TxOrphanSweepArmed {
		armed = true
	}

	out := marks.Record{
		SchemaVersion:                marks.SchemaVersion,
		MarkID:                       markID,
		StartedAt:                    startedAt,
		CompletedAt:                  opts.Now().UTC(),
		CurrentManifestVersion:       view.Header.ManifestVersion,
		CurrentManifestObjectVersion: view.Version.Token,
		RetentionSeconds:             opts.RetentionSeconds,
		TxOrphanSweepArmed:           armed,
	}
	if hasPrev {
		out.PreviousMarkID = prev.MarkID
	}

	prevPackByKey := map[string]marks.PackCandidate{}
	prevTxByKey := map[string]marks.TxCandidate{}
	prevIdxByKey := map[string]marks.IndexCandidate{}
	if hasPrev {
		for _, p := range prev.Candidates.CanonicalPacks {
			prevPackByKey[p.Key] = p
		}
		for _, t := range prev.Candidates.TxRecords {
			prevTxByKey[t.Key] = t
		}
		for _, i := range prev.Candidates.Indexes {
			prevIdxByKey[i.Key] = i
		}
	}

	now := startedAt
	for _, key := range packCandKeys {
		if old, ok := prevPackByKey[key]; ok {
			out.Candidates.CanonicalPacks = append(out.Candidates.CanonicalPacks, old)
			continue
		}
		entry := marks.PackCandidate{
			Key:                    key,
			FirstSeenUnreachableAt: now,
			MarkManifestVersion:    view.Header.ManifestVersion,
		}
		if hasPrev {
			lsr := prev.CompletedAt
			entry.LastSeenReachableAt = &lsr
		}
		out.Candidates.CanonicalPacks = append(out.Candidates.CanonicalPacks, entry)
	}
	for _, key := range indexCandKeys {
		if old, ok := prevIdxByKey[key]; ok {
			out.Candidates.Indexes = append(out.Candidates.Indexes, old)
			continue
		}
		out.Candidates.Indexes = append(out.Candidates.Indexes, marks.IndexCandidate{
			Key:                    key,
			FirstSeenUnreachableAt: now,
		})
	}
	for _, key := range txCandKeys {
		if old, ok := prevTxByKey[key]; ok {
			out.Candidates.TxRecords = append(out.Candidates.TxRecords, old)
			continue
		}
		out.Candidates.TxRecords = append(out.Candidates.TxRecords, marks.TxCandidate{
			Key:                    key,
			FirstSeenUnreachableAt: now,
		})
	}

	out.CompletedAt = opts.Now().UTC()
	return out, nil
}

// markEntropy is the goroutine-safe entropy source for mark ID minting.
// ulid.LockedMonotonicReader wraps the underlying monotonic reader with
// a sync.Mutex, matching the pattern used in internal/repo/repo.go.
var markEntropy = &ulid.LockedMonotonicReader{
	MonotonicReader: ulid.Monotonic(rand.New(rand.NewSource(time.Now().UnixNano())), 0),
}

func newMarkID(at time.Time) string {
	return "mk_" + ulid.MustNew(ulid.Timestamp(at), markEntropy).String()
}
