// Package lfs implements the Git LFS Batch API and signed-URL transfer
// machinery used by the bucketvcs gateway. The wire format is the
// standard Git LFS Batch API
// (https://github.com/git-lfs/git-lfs/blob/main/docs/api/batch.md);
// only the "basic" transfer adapter is supported.
package lfs

import "time"

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
}

// VerifyRequest is the wire shape of a POST verify body.
type VerifyRequest struct {
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}
