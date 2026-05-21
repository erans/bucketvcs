package gc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// MarkRecord is the on-disk shape of one LFS-GC mark record.
type MarkRecord struct {
	SchemaVersion         int             `json:"schema_version"`
	MarkID                string          `json:"mark_id"`
	PreviousMarkID        string          `json:"previous_mark_id,omitempty"`
	StartedAt             time.Time       `json:"started_at"`
	CompletedAt           time.Time       `json:"completed_at"`
	ManifestVersionAtMark uint64          `json:"manifest_version_at_mark"`
	RetentionSeconds      int             `json:"retention_seconds"`
	Candidates            []MarkCandidate `json:"candidates"`
}

// MarkCandidate is one orphan LFS object recorded in a mark.
type MarkCandidate struct {
	OID                     string    `json:"oid"` // 64-hex sha256
	Key                     string    `json:"key"` // full storage key
	SizeBytes               int64     `json:"size_bytes"`
	FirstSeenUnreferencedAt time.Time `json:"first_seen_unreferenced_at"`
}

// markPrefix is the storage path under which mark records live.
// Parallel to the existing internal/gc marks (under gc/marks/) so
// each kind of GC keeps its own records cleanly separated.
func markPrefix(tenant, repo string) string {
	return "tenants/" + tenant + "/repos/" + repo + "/gc/lfs-marks/"
}

func markKey(tenant, repo, markID string) string {
	return markPrefix(tenant, repo) + markID + ".json"
}

// WriteMark persists rec under tenants/<t>/repos/<r>/gc/lfs-marks/<id>.json.
// Idempotent: PutIfAbsent collapses identical re-writes. On collision
// (ErrAlreadyExists) we log a warning at slog.Default — when the
// collision is benign (idempotent re-write of the same content) the
// log is acceptable noise; when it is NOT benign (two parallel RunMark
// calls landed in the same nanosecond and computed different content),
// the warning is the only signal the second writer's data was dropped.
func WriteMark(ctx context.Context, store storage.ObjectStore, tenant, repo string, rec MarkRecord) error {
	body, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("lfs/gc: marshal mark: %w", err)
	}
	key := markKey(tenant, repo, rec.MarkID)
	if _, err := store.PutIfAbsent(ctx, key, bytes.NewReader(body), nil); err != nil {
		if errors.Is(err, storage.ErrAlreadyExists) {
			slog.Default().Warn("lfs_gc.mark_id_collision",
				"subsystem", "lfs_gc",
				"repo", tenant+"/"+repo,
				"mark_id", rec.MarkID,
				"note", "PutIfAbsent collapsed this write — if content matches an existing mark this is idempotent re-write; if not, the second writer's data was dropped",
			)
			return nil
		}
		return fmt.Errorf("lfs/gc: put mark %s: %w", key, err)
	}
	return nil
}

// ErrNoMarks is returned by ReadLatestMark when no mark records exist.
var ErrNoMarks = errors.New("lfs/gc: no marks found")

// ReadLatestMark returns the most recent mark for the repo, ordered
// by MarkID lexicographically (which is monotonically increasing
// because IDs are timestamp-prefixed). Returns ErrNoMarks if none.
func ReadLatestMark(ctx context.Context, store storage.ObjectStore, tenant, repo string) (MarkRecord, error) {
	ids, err := listMarkIDs(ctx, store, tenant, repo)
	if err != nil {
		return MarkRecord{}, err
	}
	if len(ids) == 0 {
		return MarkRecord{}, ErrNoMarks
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	return ReadMark(ctx, store, tenant, repo, ids[0])
}

// ReadMark fetches one mark by ID.
func ReadMark(ctx context.Context, store storage.ObjectStore, tenant, repo, markID string) (MarkRecord, error) {
	obj, err := store.Get(ctx, markKey(tenant, repo, markID), nil)
	if err != nil {
		return MarkRecord{}, fmt.Errorf("lfs/gc: get mark %s: %w", markID, err)
	}
	defer obj.Body.Close()
	raw, err := io.ReadAll(obj.Body)
	if err != nil {
		return MarkRecord{}, fmt.Errorf("lfs/gc: read mark %s: %w", markID, err)
	}
	var rec MarkRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return MarkRecord{}, fmt.Errorf("lfs/gc: unmarshal mark %s: %w", markID, err)
	}
	return rec, nil
}

// listMarkIDs returns every mark ID present under the repo's mark prefix.
func listMarkIDs(ctx context.Context, store storage.ObjectStore, tenant, repo string) ([]string, error) {
	var ids []string
	var token string
	prefix := markPrefix(tenant, repo)
	for {
		page, err := store.List(ctx, prefix, &storage.ListOptions{ContinuationToken: token})
		if err != nil {
			return nil, fmt.Errorf("lfs/gc: list marks: %w", err)
		}
		for _, obj := range page.Objects {
			id := strings.TrimSuffix(strings.TrimPrefix(obj.Key, prefix), ".json")
			if id != "" {
				ids = append(ids, id)
			}
		}
		if page.NextToken == "" {
			break
		}
		token = page.NextToken
	}
	return ids, nil
}

// NewMarkID generates a sortable mark identifier of the form
// "lfs-<RFC3339-utc-compact>-<nanosecond-of-second>". The 9-digit
// suffix is `time.Time.Nanosecond()` (0–999_999_999), which gives
// best-effort temporal ordering within a UTC second — substantially
// stronger than the prior `UnixNano() & 0xffffffff` scheme which
// could wrap mid-second and reorder.
//
// Strict monotonicity is NOT guaranteed: the OS clock resolution may
// repeat Nanosecond() across two calls on lower-resolution platforms,
// and even high-resolution clocks can collide if two RunMark calls
// observe the same nanosecond. A collision causes WriteMark's
// PutIfAbsent to swallow the second write as ErrAlreadyExists,
// dropping the second caller's data silently. In normal operation
// RunMark takes ≫1µs so the collision risk is negligible, but
// operators running parallel RunMark against the same repo should
// not assume strict uniqueness.
func NewMarkID(now time.Time) string {
	utc := now.UTC()
	return fmt.Sprintf("lfs-%s-%09d", utc.Format("20060102T150405Z"), utc.Nanosecond())
}
