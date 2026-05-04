package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
// Per §4.3 of the M1 design, Create is the only operation that violates
// the "tx record before CAS" ordering: it PutIfAbsent's the root first,
// then writes the create-tx record. The reason: there is no prior root
// to CAS against, and writing a tx record for a duplicate Create would
// generate a useless orphan on every accidental re-init.
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
		if errors.Is(err, storage.ErrAlreadyExists) {
			return nil, repoerrs.ErrRepoExists
		}
		return nil, fmt.Errorf("repo: create root: %w", err)
	}

	txHeader := tx.Header{
		SchemaVersion: 1, TxID: txID, RepoID: repoID,
		BaseManifestVersion: 0, BaseManifestObjectVersion: "",
		StartedAt: now,
	}
	txBody := tx.Body{Type: "create", Actor: opts.Actor}
	if err := tx.Write(ctx, store, k.TxRecordKey(txID), txHeader, txBody); err != nil {
		// The root is already on disk pointing at this tx_id. Surface
		// the error so the caller knows a repair tool will be needed
		// (M16 ships bucketvcs doctor). M8 GC will not sweep this
		// referenced-but-missing tx because root.latest_tx pins it.
		return nil, fmt.Errorf("repo: create tx record (root already committed): %w", err)
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

// txEntropy is a goroutine-safe entropy source for ulid.MustNew. ulid's
// monotonic entropy reader keeps IDs lexicographically sortable within
// a millisecond and avoids ID collisions across goroutines.
var txEntropy = ulid.Monotonic(mathrand.New(mathrand.NewSource(time.Now().UnixNano())), 0)

func newTxID() string {
	return "tx_" + ulid.MustNew(ulid.Timestamp(time.Now()), txEntropy).String()
}
