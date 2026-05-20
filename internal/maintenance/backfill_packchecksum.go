package maintenance

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/tx"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// errBackfillNothingToDo signals from the r.Commit callback that the
// CAS-pre-image manifest already has every PackChecksum populated, so
// the run is a no-op on this attempt. Outer code matches via errors.Is —
// repo.Commit wraps callback errors as
//
//	fmt.Errorf("%w: %w", repoerrs.ErrCallbackFailed, err)
//
// (see internal/repo/repo.go:271), and Go's wrap chain lets errors.Is
// walk through both wraps to find this sentinel.
var errBackfillNothingToDo = errors.New("backfill: nothing to do")

// readRemotePackTrailer downloads the last 20 bytes of the pack at key
// via a ranged GET and returns them hex-encoded (40 chars). This is the
// pack's trailer SHA-1 — Git's "pack checksum", which is also the pack's
// canonical ID for fresh packs. Used by lazy backfill of legacy
// (pre-M11) PackEntry rows where PackChecksum was not recorded at
// receive-pack / import time.
//
// size must be the total pack length in bytes (>= 20). A pack shorter
// than 20 bytes is malformed; the function returns an error in that case
// rather than reading past the start of the object.
func readRemotePackTrailer(ctx context.Context, s storage.ObjectStore, key string, size int64) (string, error) {
	if size < 20 {
		return "", fmt.Errorf("pack %q is %d bytes; trailer SHA-1 requires >= 20", key, size)
	}
	rc, err := s.GetRange(ctx, key, size-20, size-1)
	if err != nil {
		return "", fmt.Errorf("get-range trailer: %w", err)
	}
	defer rc.Close()
	var buf [20]byte
	if _, err := io.ReadFull(rc, buf[:]); err != nil {
		return "", fmt.Errorf("read trailer: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// BackfillPackChecksumsIfNeeded inspects m and, if any PackEntry has an
// empty PackChecksum, downloads the trailing 20 bytes of each affected
// pack and CAS-merges the populated rows back into the manifest.
//
// Returns the (possibly-updated) body, a boolean indicating whether a
// commit actually landed, and any non-recoverable error. A per-pack
// trailer read failure is logged at WARN and skipped — the run does not
// abort, since a future maintenance pass may succeed where this one did
// not (e.g., a transient storage hiccup). If no pack still needs
// backfilling on a given CAS attempt (because a concurrent run filled
// them all), the function returns (m, false, nil) instead of writing a
// no-op tx.
//
// actor is the tx.Body Actor string ("u_op" in production maintenance
// runs). casRetry bounds the CAS attempts; non-positive defaults to
// DefaultCASRetry. A nil logger defaults to slog.Default().
//
// This is the lazy migration path for pre-M11 manifests where the
// importer / receive-pack did not record the trailer SHA-1 at write
// time. Newly produced packs (Phase 5.1) already carry PackChecksum at
// the repack write boundary, so steady-state runs short-circuit on the
// fast path.
func BackfillPackChecksumsIfNeeded(
	ctx context.Context,
	s storage.ObjectStore,
	r *repo.Repo,
	m manifest.Body,
	casRetry int,
	actor string,
	logger *slog.Logger,
) (manifest.Body, bool, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if casRetry <= 0 {
		casRetry = DefaultCASRetry
	}
	if actor == "" {
		actor = "u_op"
	}

	// Fast path: nothing to do if every pack already has a checksum.
	// Avoids any tx record on the common case.
	needed := false
	for _, p := range m.Packs {
		if p.PackChecksum == "" {
			needed = true
			break
		}
	}
	if !needed {
		return m, false, nil
	}

	var out manifest.Body
	txBody := tx.Body{Type: "maintenance_backfill_pack_checksum", Actor: actor}
	_, err := r.Commit(ctx, txBody, func(prev *repo.RootView) ([]byte, error) {
		body, uerr := manifest.UnmarshalBody(prev.Body)
		if uerr != nil {
			return nil, fmt.Errorf("backfill: unmarshal: %w", uerr)
		}
		anyFilled := false
		for i, p := range body.Packs {
			if p.PackChecksum != "" {
				continue
			}
			sum, terr := readRemotePackTrailer(ctx, s, p.PackKey, p.SizeBytes)
			if terr != nil {
				// Context cancellation must propagate — it's not a "transient
				// trailer hiccup", and silently WARN-logging once per remaining
				// pack on a cancelled run produces a noisy flurry of warnings
				// followed by a commit of whatever subset filled in first.
				if errors.Is(terr, context.Canceled) || errors.Is(terr, context.DeadlineExceeded) {
					return nil, terr
				}
				logger.WarnContext(ctx, "backfill: pack trailer read failed (skipping)",
					slog.String("pack_id", p.PackID),
					slog.String("pack_key", p.PackKey),
					slog.String("err", terr.Error()))
				continue
			}
			body.Packs[i].PackChecksum = sum
			anyFilled = true
		}
		if !anyFilled {
			return nil, errBackfillNothingToDo
		}
		out = body
		return manifest.MarshalBody(body)
	}, repo.WithCommitPolicy(repo.CommitPolicy{MaxRetries: casRetry}))
	if errors.Is(err, errBackfillNothingToDo) {
		return m, false, nil
	}
	if err != nil {
		return m, false, fmt.Errorf("backfill: %w", err)
	}
	return out, true, nil
}
