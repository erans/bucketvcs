// Package lfs implements the Git LFS Batch API and signed-URL transfer
// machinery used by the bucketvcs gateway. The wire format is the
// standard Git LFS Batch API
// (https://github.com/git-lfs/git-lfs/blob/main/docs/api/batch.md);
// only the "basic" transfer adapter is supported.
package lfs

import (
	"time"

	"github.com/bucketvcs/bucketvcs/internal/lfs/quota"
)

// ObjectRef is one object in a batch request.
type ObjectRef struct {
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}

// BatchRequest is the wire shape of a POST .../info/lfs/objects/batch body.
type BatchRequest struct {
	Operation string      `json:"operation"`
	Transfers []string    `json:"transfers,omitempty"`
	Objects   []ObjectRef `json:"objects"`
}

// Action describes one of the actions returned for an object: "upload",
// "download", or "verify".
type Action struct {
	Href      string            `json:"href"`
	Header    map[string]string `json:"header,omitempty"`
	ExpiresAt *time.Time        `json:"expires_at,omitempty"`
}

// ObjectError is the per-object error returned inside a batch response.
type ObjectError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ObjectAction is one entry in a batch response. Exactly one of Actions
// or Error is populated.
type ObjectAction struct {
	OID     string            `json:"oid"`
	Size    int64             `json:"size"`
	Actions map[string]Action `json:"actions,omitempty"`
	Error   *ObjectError      `json:"error,omitempty"`
}

// BatchResponse is the wire shape of the batch endpoint response.
type BatchResponse struct {
	Transfer string         `json:"transfer"`
	Objects  []ObjectAction `json:"objects"`

	// QuotaError, if non-nil, carries the structured *quota.QuotaError
	// values for an M13.5 quota-rejected batch. NOT serialized to the
	// wire (the per-object 507 errors are what the client sees);
	// exposed so handleBatch can emit the lfs.quota.exceeded audit
	// without re-querying the authdb. The json:"-" tag keeps the
	// API surface unchanged for stock git-lfs clients.
	QuotaError *quota.QuotaError `json:"-"`
}

// VerifyRequest is the wire shape of a POST verify body.
type VerifyRequest struct {
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}

// --- LFS Locks API wire format (M13.3) ---
// Per the LFS Locks spec: https://github.com/git-lfs/git-lfs/blob/main/docs/api/locking.md

// LockOwner is the owner sub-object in lock responses. Per spec, only
// "name" is sent on the wire (the user ID is server-internal).
type LockOwner struct {
	Name string `json:"name"`
}

// LockWire is the wire-format lock object. Distinct from
// internal/lfs/locks.Lock which carries server-internal fields.
type LockWire struct {
	ID       string    `json:"id"`
	Path     string    `json:"path"`
	LockedAt time.Time `json:"locked_at"`
	Owner    LockOwner `json:"owner"`
}

// LockRefSpec is the optional ref-scoping nub on a lock request body.
type LockRefSpec struct {
	Name string `json:"name"`
}

// LockRequest is POST /info/lfs/locks.
type LockRequest struct {
	Path string       `json:"path"`
	Ref  *LockRefSpec `json:"ref,omitempty"`
}

// LockResponse is the 201 body of a successful lock create.
type LockResponse struct {
	Lock LockWire `json:"lock"`
}

// LockConflictResponse is the 409 body when a lock already exists for
// the requested path.
type LockConflictResponse struct {
	Lock    LockWire `json:"lock"`
	Message string   `json:"message"`
}

// ListLocksResponse is GET /info/lfs/locks.
type ListLocksResponse struct {
	Locks      []LockWire `json:"locks"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

// LocksVerifyRequest is POST /info/lfs/locks/verify. Distinct from
// the batch-flow VerifyRequest above (which carries {oid, size}).
type LocksVerifyRequest struct {
	Cursor string       `json:"cursor,omitempty"`
	Limit  int          `json:"limit,omitempty"`
	Ref    *LockRefSpec `json:"ref,omitempty"`
}

// LocksVerifyResponse partitions ours/theirs.
type LocksVerifyResponse struct {
	Ours       []LockWire `json:"ours"`
	Theirs     []LockWire `json:"theirs"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

// UnlockRequest is POST /info/lfs/locks/<id>/unlock.
type UnlockRequest struct {
	Force bool `json:"force,omitempty"`
}

// UnlockResponse echoes the deleted lock so the client knows what
// was removed (useful when force was used).
type UnlockResponse struct {
	Lock LockWire `json:"lock"`
}
