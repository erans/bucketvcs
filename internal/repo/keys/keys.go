// Package keys owns the §6 durable-key naming contract for bucketvcs
// repositories. Every path inside /tenants/{tid}/repos/{rid}/ is
// constructed here; M2/M3/M8 do not invent paths.
package keys

import (
	"regexp"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/repo"
)

// Repo holds the pre-computed key prefix for one (tenant, repo) pair.
// Construct via NewRepo, which validates IDs.
type Repo struct {
	tenantID, repoID string
	prefix           string
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// NewRepo validates IDs and returns a Repo bound to the corresponding
// key prefix.
func NewRepo(tenantID, repoID string) (*Repo, error) {
	if !validID(tenantID) {
		return nil, repo.ErrInvalidTenantID
	}
	if !validID(repoID) {
		return nil, repo.ErrInvalidRepoID
	}
	return &Repo{
		tenantID: tenantID,
		repoID:   repoID,
		prefix:   "tenants/" + tenantID + "/repos/" + repoID + "/",
	}, nil
}

// Prefix returns the durable key prefix for this repo, with trailing
// slash. All keys returned by this package's constructors begin with
// this prefix.
func (r *Repo) Prefix() string { return r.prefix }

// TenantID returns the tenant identifier this Repo was constructed with.
func (r *Repo) TenantID() string { return r.tenantID }

// RepoID returns the repo identifier this Repo was constructed with.
func (r *Repo) RepoID() string { return r.repoID }

// RootManifestKey returns the durable key for the §7 root manifest.
func (r *Repo) RootManifestKey() string {
	return r.prefix + "manifest/root.json"
}

// TxRecordKey returns the durable key for one §8 immutable transaction
// record identified by txID (a ULID minted by Commit).
func (r *Repo) TxRecordKey(txID string) string {
	return r.prefix + "tx/" + txID + ".json"
}

// TxPrefix returns the prefix for listing all tx records in this repo.
// Used by M8 GC for orphan sweeps; not used by M1 itself.
func (r *Repo) TxPrefix() string {
	return r.prefix + "tx/"
}

func validID(s string) bool {
	if !idPattern.MatchString(s) {
		return false
	}
	// Reject path-traversal and dot-only segments even within the
	// allowed charset.
	if s == "." || s == ".." || strings.Contains(s, "..") {
		return false
	}
	return true
}

// CanonicalPackKey returns the path for a canonical (.pack) object in the
// canonical area. Used by M2.
func (r *Repo) CanonicalPackKey(packHash string) string {
	return r.prefix + "packs/canonical/" + packHash + ".pack"
}

// GeneratedPackKey returns the path for an on-the-fly generated pack.
// Used by M2.
func (r *Repo) GeneratedPackKey(packHash string) string {
	return r.prefix + "packs/generated/" + packHash + ".pack"
}

// PackIdxKey returns the .idx path for a pack in the named area
// ("canonical" or "generated"). Panics on unknown area to catch typos
// at test time. Used by M2.
func (r *Repo) PackIdxKey(packHash, area string) string {
	checkPackArea(area)
	return r.prefix + "packs/" + area + "/" + packHash + ".idx"
}

// PackBitmapKey returns the .bitmap path for a pack in the named area.
// Used by M2.
func (r *Repo) PackBitmapKey(packHash, area string) string {
	checkPackArea(area)
	return r.prefix + "packs/" + area + "/" + packHash + ".bitmap"
}

func checkPackArea(area string) {
	if area != "canonical" && area != "generated" {
		panic("keys: unknown pack area " + area + ` (want "canonical" or "generated")`)
	}
}

// CommitGraphKey returns the path for a commit-graph index. Used by M2.
func (r *Repo) CommitGraphKey(graphHash string) string {
	return r.prefix + "indexes/commit-graphs/" + graphHash + ".graph"
}

// ReachabilityKey returns the path for a reachability index. Used by M2.
func (r *Repo) ReachabilityKey(indexHash string) string {
	return r.prefix + "indexes/reachability/" + indexHash + ".json"
}

// BundleKey returns the path for a bundle file. Used by M11.
func (r *Repo) BundleKey(bundleID string) string {
	return r.prefix + "bundles/" + bundleID + ".bundle"
}

// BundleManifestKey returns the path for a bundle's sidecar manifest.
// Used by M11.
func (r *Repo) BundleManifestKey(bundleID string) string {
	return r.prefix + "bundles/" + bundleID + ".json"
}

// LFSObjectKey returns the path for an LFS object. Used by M13.
func (r *Repo) LFSObjectKey(sha256 string) string {
	return r.prefix + "lfs/objects/" + sha256
}

// HookKey returns the path for a server-side hook payload. Used by M14.
func (r *Repo) HookKey(hookID, name string) string {
	return r.prefix + "hooks/" + hookID + "/" + name
}

// GCMarkKey returns the path for a GC mark record. Used by M8.
func (r *Repo) GCMarkKey(markID string) string {
	return r.prefix + "gc/marks/" + markID + ".json"
}

// GCSweepKey returns the path for a GC sweep record. Used by M8.
func (r *Repo) GCSweepKey(sweepID string) string {
	return r.prefix + "gc/sweeps/" + sweepID + ".json"
}

// RefShardKey returns the path for a sharded-refs shard. Used by M12;
// never written by M1.
func (r *Repo) RefShardKey(shardHash string) string {
	return r.prefix + "manifest/ref-shards/" + shardHash + ".json"
}
