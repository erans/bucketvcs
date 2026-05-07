// Package tx defines the M1-owned tx-record header struct, the
// caller-supplied body struct, and helpers to marshal a complete
// immutable transaction record per spec §8.
package tx

import (
	"encoding/json"
	"fmt"
	"time"
)

// Header is the M1-owned subset of a tx record. M1 mints these fields
// at commit time; callers cannot supply them.
type Header struct {
	SchemaVersion             int       `json:"schema_version"`
	TxID                      string    `json:"tx_id"`
	RepoID                    string    `json:"repo_id"`
	BaseManifestVersion       uint64    `json:"base_manifest_version"`
	BaseManifestObjectVersion string    `json:"base_manifest_object_version"`
	StartedAt                 time.Time `json:"started_at"`
}

// Body is the caller-supplied subset of a tx record. M1 splices these
// fields into the top level of the JSON document at write time.
type Body struct {
	// Type is the high-level operation classification: "create",
	// "push", "gc", or future values defined by M2/M3/M8.
	Type string `json:"type"`

	// Actor identifies the principal performing the operation. M4 will
	// supply real identity strings; M1 / unit tests may use placeholder
	// values like "u_test".
	Actor string `json:"actor"`

	// RefUpdates, NewPacks, Validation are opaque to M1; their schemas
	// are defined by the milestones that produce them.
	RefUpdates json.RawMessage `json:"ref_updates,omitempty"`
	NewPacks   json.RawMessage `json:"new_packs,omitempty"`
	Validation json.RawMessage `json:"validation,omitempty"`

	// Extra carries forward-compatible additional fields. Must be a
	// JSON object whose keys do not collide with any reserved header or
	// known body key. Marshal returns an error on collision.
	Extra json.RawMessage `json:"-"`
}

// headerKeys lists the JSON field names M1 owns at the top level of a
// tx record. Used by Marshal to reject body content that would shadow
// these fields. Unexported to prevent mutation by callers.
var headerKeys = []string{
	"schema_version", "tx_id", "repo_id",
	"base_manifest_version", "base_manifest_object_version", "started_at",
}

// bodyKnownKeys lists the JSON field names the Body struct emits. Used
// by Marshal to reject Extra content that would shadow these fields.
var bodyKnownKeys = []string{"type", "actor", "ref_updates", "new_packs", "validation"}

// HeaderKeyList returns a fresh copy of the reserved header field names.
// External callers that need to inspect the M1-owned tx-record header
// schema should use this rather than the unexported backing slice.
func HeaderKeyList() []string {
	return append([]string(nil), headerKeys...)
}

// Marshal returns the canonical JSON bytes of a complete tx record:
// header keys + body fields + Extra, all flattened to a single
// top-level object.
func Marshal(h Header, b Body) ([]byte, error) {
	top := map[string]json.RawMessage{}

	hb, err := json.Marshal(h)
	if err != nil {
		return nil, fmt.Errorf("tx: marshal header: %w", err)
	}
	if err := json.Unmarshal(hb, &top); err != nil {
		return nil, fmt.Errorf("tx: re-parse header: %w", err)
	}

	bb, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("tx: marshal body: %w", err)
	}
	var bodyMap map[string]json.RawMessage
	if err := json.Unmarshal(bb, &bodyMap); err != nil {
		return nil, fmt.Errorf("tx: re-parse body: %w", err)
	}
	for k, v := range bodyMap {
		if _, dup := top[k]; dup {
			return nil, fmt.Errorf("tx: body key %q collides with header", k)
		}
		top[k] = v
	}

	if len(b.Extra) > 0 {
		var extraMap map[string]json.RawMessage
		if err := json.Unmarshal(b.Extra, &extraMap); err != nil {
			return nil, fmt.Errorf("tx: Extra must be a JSON object: %w", err)
		}
		if extraMap == nil {
			return nil, fmt.Errorf("tx: Extra must be a JSON object, got null")
		}
		reserved := reservedKeySet()
		for k, v := range extraMap {
			if _, isReserved := reserved[k]; isReserved {
				return nil, fmt.Errorf("tx: extra key %q collides with header or known body key", k)
			}
			top[k] = v
		}
	}
	return json.Marshal(top)
}

// reservedKeySet returns a fresh map of every JSON field name M1 owns
// at the top level of a tx record (headerKeys ∪ bodyKnownKeys). Built
// per call to keep callers from mutating shared state.
func reservedKeySet() map[string]struct{} {
	out := make(map[string]struct{}, len(headerKeys)+len(bodyKnownKeys))
	for _, k := range headerKeys {
		out[k] = struct{}{}
	}
	for _, k := range bodyKnownKeys {
		out[k] = struct{}{}
	}
	return out
}
