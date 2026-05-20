package locks

import "errors"

// ErrAlreadyLocked is returned by Store.Create when a lock already
// exists for (tenant, repo, path). The handler maps this to HTTP 409
// per the LFS spec, and re-reads the existing lock to include it in
// the LockConflictResponse body.
var ErrAlreadyLocked = errors.New("locks: a lock for that path already exists")

// ErrNotFound is returned by Store.Get and Store.Delete when no lock
// with the given (tenant, repo, id) tuple exists. The handler maps
// this to HTTP 404.
var ErrNotFound = errors.New("locks: lock not found")

// ErrBadCursor is returned by Store.List / Store.Verify when the
// cursor string is malformed (not a non-negative integer). The
// handler maps this to HTTP 400.
var ErrBadCursor = errors.New("locks: bad cursor")
