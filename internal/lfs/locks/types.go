// Package locks implements the Git LFS Locking API data model.
// Per-repo lock records persist in a sqlite table (lfs_locks) on the
// existing M4 authdb. The package exposes a Store with the CRUD +
// verify-partition primitives the LFS handler needs; the handler is
// in internal/lfs.
package locks

import "time"

// Lock is a single LFS lock record as exposed to handlers and wire
// callers. Owner.Name and Owner.UserID are filled by the store via
// a join against the users table.
type Lock struct {
	ID       string
	Tenant   string
	Repo     string
	Path     string
	RefName  string // empty = repo-wide
	Owner    LockOwner
	LockedAt time.Time
}

// LockOwner identifies the user who created a lock. Both fields are
// populated for handler responses; only UserID is persisted in the
// lfs_locks row (Name comes from the users table join at read time).
type LockOwner struct {
	UserID string
	Name   string
}

// VerifyResult partitions a list of locks by ownership relative to a
// caller. Ours = locks owned by the caller. Theirs = locks owned by
// any other user. Per LFS spec, both lists are independently
// paginated; we use a single shared NextCursor for simplicity (the
// spec allows this — most servers do the same).
type VerifyResult struct {
	Ours       []Lock
	Theirs     []Lock
	NextCursor string
}

// ListOptions controls List + Verify.
type ListOptions struct {
	// Filter: empty fields are not applied.
	Path string
	ID   string
	// RefName: when non-empty, returns locks where ref_name = RefName
	// OR ref_name IS NULL (the null case is "applies to all refs").
	RefName string

	// Pagination.
	Cursor string // opaque; empty for first page
	Limit  int    // 0 = use default (100); max MaxLimit (1000)
}

const (
	defaultLimit = 100
	MaxLimit     = 1000
)
