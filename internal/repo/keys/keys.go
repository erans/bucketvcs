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
