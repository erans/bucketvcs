package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	mathrand "math/rand"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/repoerrs"
	"github.com/bucketvcs/bucketvcs/internal/repo/tx"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/oklog/ulid/v2"
)

// Repo is a handle to one (tenant, repo) pair backed by an
// ObjectStore. Construct via Open or Create. Repo holds no per-call
// mutable state and is safe to share between goroutines.
type Repo struct {
	store storage.ObjectStore
	keys  *keys.Repo
}

// Open returns a handle for an existing repo. Errors:
//   - ErrInvalidTenantID / ErrInvalidRepoID if the IDs fail validation.
//   - ErrRepoNotFound if the root manifest is missing.
//   - ErrUnsupportedSchema if the manifest's header fails the §43.7 gate.
//   - wrapped storage error otherwise.
//
// Open does not create anything. Use Create (Task 11) to initialize a
// new repo.
func Open(ctx context.Context, store storage.ObjectStore, tenantID, repoID string) (*Repo, error) {
	k, err := keys.NewRepo(tenantID, repoID)
	if err != nil {
		return nil, err
	}
	if _, _, _, err := manifest.ReadRoot(ctx, store, k.RootManifestKey()); err != nil {
		return nil, err
	}
	return &Repo{store: store, keys: k}, nil
}

// TenantID returns the tenant identifier this Repo was opened with.
func (r *Repo) TenantID() string { return r.keys.TenantID() }

// RepoID returns the repo identifier this Repo was opened with.
func (r *Repo) RepoID() string { return r.keys.RepoID() }

// CreateOptions controls Create-time choices.
type CreateOptions struct {
	// DefaultBranch is the body-level default_branch field. Defaults
	// to "refs/heads/main" when empty.
	DefaultBranch string

	// ObjectFormat is the Git object format. M1 supports "sha1" only;
	// "sha256" is reserved. Defaults to "sha1".
	ObjectFormat string

	// Actor is recorded in the create tx record. Defaults to "unknown".
	Actor string
}

// Create writes the initial tx record + root manifest for a new repo.
// Returns ErrRepoExists if the root manifest already exists.
//
// Create writes the tx record first (orphan-on-duplicate is acceptable;
// M8 GC sweeps it). The root PutIfAbsent is the commit point. This
// ordering eliminates the partial-init failure mode where the root
// references a non-existent tx.
func Create(ctx context.Context, store storage.ObjectStore, tenantID, repoID string, opts CreateOptions) (*Repo, error) {
	k, err := keys.NewRepo(tenantID, repoID)
	if err != nil {
		return nil, err
	}
	if opts.DefaultBranch == "" {
		opts.DefaultBranch = "refs/heads/main"
	}
	if opts.ObjectFormat == "" {
		opts.ObjectFormat = "sha1"
	}
	if opts.ObjectFormat != "sha1" {
		return nil, fmt.Errorf("repo: object_format %q not supported in M1 (only sha1)", opts.ObjectFormat)
	}
	if opts.Actor == "" {
		opts.Actor = "unknown"
	}

	now := time.Now().UTC().Truncate(time.Second)
	txID := newTxID()

	txHeader := tx.Header{
		SchemaVersion: 1, TxID: txID, RepoID: repoID,
		BaseManifestVersion: 0, BaseManifestObjectVersion: "",
		StartedAt: now,
	}
	txBody := tx.Body{Type: "create", Actor: opts.Actor}
	if err := tx.Write(ctx, store, k.TxRecordKey(txID), txHeader, txBody); err != nil {
		return nil, fmt.Errorf("repo: create tx record: %w", err)
	}

	header := manifest.RootHeader{
		SchemaVersion:    manifest.CurrentSchemaVersion,
		MinReaderVersion: manifest.SupportedReaderVersion,
		RepoID:           repoID,
		RepoFormat: manifest.Format{
			ObjectFormat:  opts.ObjectFormat,
			Compatibility: []string{opts.ObjectFormat},
		},
		ManifestVersion: 1,
		LatestTx:        txID,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	body := json.RawMessage(fmt.Sprintf(
		`{"refs":{},"packs":[],"indexes":{},"bundles":[],"default_branch":%q}`,
		opts.DefaultBranch,
	))
	rootBytes, err := manifest.WrapHeaderInBody(header, body)
	if err != nil {
		return nil, err
	}

	if _, err := store.PutIfAbsent(ctx, k.RootManifestKey(),
		strings.NewReader(string(rootBytes)), nil); err != nil {
		// Orphan tx record is acceptable; M8 GC sweeps it.
		if errors.Is(err, storage.ErrAlreadyExists) {
			return nil, repoerrs.ErrRepoExists
		}
		return nil, fmt.Errorf("repo: create root: %w", err)
	}

	return &Repo{store: store, keys: k}, nil
}

// RootView is a snapshot of the root manifest as returned by ReadRoot.
type RootView struct {
	Header    manifest.RootHeader
	Body      json.RawMessage
	Version   storage.ObjectVersion
	SizeBytes int64
}

// ReadRoot returns the current root manifest header + body bytes +
// version token. Errors as for manifest.ReadRoot:
//   - ErrRepoNotFound if the root is missing.
//   - ErrUnsupportedSchema if the gate refuses the manifest.
func (r *Repo) ReadRoot(ctx context.Context) (*RootView, error) {
	header, body, ver, err := manifest.ReadRoot(ctx, r.store, r.keys.RootManifestKey())
	if err != nil {
		return nil, err
	}
	return &RootView{
		Header:    header,
		Body:      body,
		Version:   ver,
		SizeBytes: int64(len(body)),
	}, nil
}

// txEntropy is the goroutine-safe entropy source for ulid.MustNew.
// ulid.Monotonic alone is NOT safe under concurrent calls (it mutates
// internal state without locking); LockedMonotonicReader wraps it with
// the sync.Mutex required for concurrent ID minting from Commit
// retries and parallel Create/Commit calls.
var txEntropy = &ulid.LockedMonotonicReader{
	MonotonicReader: ulid.Monotonic(mathrand.New(mathrand.NewSource(time.Now().UnixNano())), 0),
}

func newTxID() string {
	return "tx_" + ulid.MustNew(ulid.Timestamp(time.Now()), txEntropy).String()
}

// CommitPolicy controls retry behavior for a single Commit invocation.
// Zero MaxRetries is normalized to 1 (at least one attempt always runs);
// zero BackoffBase disables backoff between attempts.
type CommitPolicy struct {
	// MaxRetries is the maximum number of CAS attempts before
	// returning *CommitGaveUpError. Default 8. Normalized to at least 1.
	MaxRetries int

	// BackoffBase is the base delay between retries. Actual delay is
	// jittered uniformly in [0, BackoffBase * 2^attempt). Default 5ms.
	// Set to 0 to disable backoff (useful in tests).
	BackoffBase time.Duration
}

// CommitOption configures one Commit invocation.
type CommitOption func(*CommitPolicy)

// WithCommitPolicy overrides the default CommitPolicy for one Commit.
func WithCommitPolicy(p CommitPolicy) CommitOption {
	return func(out *CommitPolicy) { *out = p }
}

const (
	defaultMaxRetries  = 8
	defaultBackoffBase = 5 * time.Millisecond
)

// Commit performs the §8 atomic-pair (write tx record, then CAS root)
// with bounded retry on CAS conflict. Each attempt mints a fresh tx_id
// so every tx record on disk has accurate base_manifest_* fields. The
// returned tx_id is the *winning* one (referenced by the new root).
// Lost attempts leave orphan tx records on disk for M8 GC.
//
// Errors:
//   - context.Canceled / DeadlineExceeded if ctx cancels.
//   - ErrRepoNotFound if the root has been deleted out from under us.
//   - ErrUnsupportedSchema if a future-schema manifest landed.
//   - wrapped ErrCallbackFailed if buildBody returns an error.
//   - *CommitGaveUpError if the retry budget exhausts on CAS conflicts.
//   - other wrapped storage errors otherwise.
func (r *Repo) Commit(
	ctx context.Context,
	txBody tx.Body,
	buildBody func(prev *RootView) (newBody []byte, err error),
	opts ...CommitOption,
) (string, error) {
	policy := CommitPolicy{MaxRetries: defaultMaxRetries, BackoffBase: defaultBackoffBase}
	for _, o := range opts {
		o(&policy)
	}
	// Normalize: a non-positive MaxRetries is degenerate (Commit would
	// immediately return CommitGaveUpError with zero attempts), so
	// silently raise to at least 1. Callers wanting the documented
	// "disable backoff" semantics can set BackoffBase=0 without also
	// zeroing MaxRetries.
	if policy.MaxRetries < 1 {
		policy.MaxRetries = 1
	}

	var (
		orphans []string
		lastErr error
	)
	for attempt := 1; attempt <= policy.MaxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}

		view, err := r.ReadRoot(ctx)
		if err != nil {
			return "", err
		}
		// Snapshot M1-owned fields BEFORE calling the user callback.
		// A buggy or hostile callback could mutate view.Header or
		// view.Version; downstream code uses these snapshots so any
		// such mutation cannot corrupt the tx record's base_* fields,
		// the next manifest header, or the CAS pre-image version.
		snapHeader := view.Header
		snapVersion := view.Version
		// Defensively clone Compatibility slice header to prevent
		// aliasing if a callback mutates the backing array.
		if len(snapHeader.RepoFormat.Compatibility) > 0 {
			snapHeader.RepoFormat.Compatibility = append([]string(nil), snapHeader.RepoFormat.Compatibility...)
		}
		newBody, err := buildBody(view)
		if err != nil {
			return "", fmt.Errorf("%w: %w", repoerrs.ErrCallbackFailed, err)
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}

		txID := newTxID()
		txHeader := tx.Header{
			SchemaVersion:             1,
			TxID:                      txID,
			RepoID:                    r.RepoID(),
			BaseManifestVersion:       snapHeader.ManifestVersion,
			BaseManifestObjectVersion: snapVersion.Token,
			StartedAt:                 time.Now().UTC().Truncate(time.Second),
		}
		if err := tx.Write(ctx, r.store, r.keys.TxRecordKey(txID), txHeader, txBody); err != nil {
			return "", err
		}

		nextHeader := snapHeader
		nextHeader.ManifestVersion++
		nextHeader.LatestTx = txID
		nextHeader.UpdatedAt = time.Now().UTC().Truncate(time.Second)

		nextBytes, err := manifest.WrapHeaderInBody(nextHeader, newBody)
		if err != nil {
			return "", err
		}

		if err := ctx.Err(); err != nil {
			return "", err
		}

		if _, err := manifest.CASRoot(ctx, r.store, r.keys.RootManifestKey(), nextBytes, snapVersion); err != nil {
			lastErr = err
			orphans = append(orphans, txID)
			if errors.Is(err, storage.ErrVersionMismatch) {
				// Only back off if there will be another attempt.
				// Backoff after the final attempt is wasted and could
				// turn a cancellation into a misleading
				// CommitGaveUpError.
				if attempt < policy.MaxRetries && policy.BackoffBase > 0 {
					if berr := sleepBackoff(ctx, policy.BackoffBase, attempt); berr != nil {
						return "", berr
					}
				}
				continue
			}
			return "", err
		}
		return txID, nil
	}
	return "", &repoerrs.CommitGaveUpError{
		Attempts: policy.MaxRetries, OrphanTxIDs: orphans, LastErr: lastErr,
	}
}

// maxBackoffShift caps the exponential growth of the backoff multiplier
// before any shift, so int64 cannot overflow. 1<<10 = 1024x base is
// plenty of jitter range for realistic CAS contention.
const maxBackoffShift = 10

func sleepBackoff(ctx context.Context, base time.Duration, attempt int) error {
	if base <= 0 {
		return nil
	}
	shift := attempt
	if shift < 0 {
		shift = 0
	}
	if shift > maxBackoffShift {
		shift = maxBackoffShift
	}
	mult := int64(1) << uint(shift) // safe: shift <= 10
	// Saturating multiply: clamp to MaxInt64 if base*mult would overflow.
	upper := int64(base)
	if upper > 0 && upper > math.MaxInt64/mult {
		upper = math.MaxInt64
	} else {
		upper = upper * mult
	}
	if upper <= 0 {
		// Pathological input (negative base shouldn't happen, base==0
		// already returned above). Don't panic mathrand.Int63n.
		return nil
	}
	jitter := time.Duration(mathrand.Int63n(upper))
	t := time.NewTimer(jitter)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
