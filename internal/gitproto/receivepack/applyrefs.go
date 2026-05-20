package receivepack

import (
	"context"
	"os"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
	"github.com/bucketvcs/bucketvcs/internal/mirror"
	"github.com/bucketvcs/bucketvcs/internal/repo/oidconst"
)

// applyRefUpdateInBare dispatches a single ref change (create/update/
// delete) directly via gitcli against the bare repo. Mirrors the logic
// of mirror.applyRefUpdate (which is unexported) so we can apply refs
// to the bare BEFORE calling importer.BuildAndCommit, whose repack
// requires the new tips to be reachable from a ref.
func applyRefUpdateInBare(ctx context.Context, bareDir string, u mirror.RefUpdate) error {
	switch {
	case u.NewOID == oidconst.NullOIDHex:
		return gitcli.UpdateRefDelete(ctx, bareDir, u.Refname, u.OldOID)
	case u.OldOID == "" || u.OldOID == oidconst.NullOIDHex:
		return gitcli.UpdateRef(ctx, bareDir, u.Refname, u.NewOID)
	default:
		return gitcli.UpdateRefCAS(ctx, bareDir, u.Refname, u.NewOID, u.OldOID)
	}
}

// markMirrorStale removes the mirror's manifest-version sentinel so the
// next SyncToCurrent treats the mirror as not-current and rebuilds from
// the authoritative bucket state. Used on every failure path AFTER we
// have mutated the local bare (applied refs) but BEFORE the bucket has
// been updated to match — without this, the unchanged sentinel would
// match the unchanged bucket version and SyncToCurrent would falsely
// consider the mirror current while the bare carried partially-applied
// or never-committed refs.
//
// Best-effort: a failure here is logged via the error return being
// dropped. The next read attempt will fail to parse the (possibly
// still-present) sentinel and rebuild anyway, since the file content
// after a partial os.Remove failure is undefined and readSentinel
// treats any error as "not current".
func markMirrorStale(m *mirror.Mirror) {
	_ = os.Remove(m.VersionFile())
}
