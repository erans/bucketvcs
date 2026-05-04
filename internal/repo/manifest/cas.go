package manifest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/repo/repoerrs"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// ReadRoot fetches the root manifest at key, parses out the M1-owned
// header, and applies the §43.7 schema gate. Returns the parsed header,
// the opaque body bytes (everything except header keys), and the
// storage version token for a later CAS. Errors:
//   - repo.ErrRepoNotFound if the object does not exist.
//   - repo.ErrUnsupportedSchema if the header's schema_version or
//     min_reader_version exceeds what this build supports.
//   - wrapped storage error otherwise.
//
// The schema gate is applied here so every reader (Open, Commit's
// retry loop, future doctor tools) gets fail-closed semantics for free.
func ReadRoot(ctx context.Context, s storage.ObjectStore, key string) (
	RootHeader, json.RawMessage, storage.ObjectVersion, error,
) {
	obj, err := s.Get(ctx, key, nil)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return RootHeader{}, nil, storage.ObjectVersion{}, repoerrs.ErrRepoNotFound
		}
		return RootHeader{}, nil, storage.ObjectVersion{}, fmt.Errorf("repo: read root: %w", err)
	}
	defer obj.Body.Close()

	raw, err := io.ReadAll(obj.Body)
	if err != nil {
		return RootHeader{}, nil, storage.ObjectVersion{}, fmt.Errorf("repo: read root body: %w", err)
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return RootHeader{}, nil, storage.ObjectVersion{}, fmt.Errorf("repo: parse root manifest: %w", err)
	}

	headerJSON, err := pickHeader(top)
	if err != nil {
		return RootHeader{}, nil, storage.ObjectVersion{}, err
	}

	// Per §43.7, run the schema gate on a minimal compatibility header
	// BEFORE parsing the full RootHeader. This way a future-schema
	// manifest whose other header fields changed type (e.g.
	// manifest_version uint64 → string) still fails with
	// ErrUnsupportedSchema, not a generic parse error.
	var compat compatHeader
	if err := json.Unmarshal(headerJSON, &compat); err != nil {
		return RootHeader{}, nil, storage.ObjectVersion{}, fmt.Errorf("repo: parse compat header: %w", err)
	}
	if err := SchemaGate(RootHeader{
		SchemaVersion:    compat.SchemaVersion,
		MinReaderVersion: compat.MinReaderVersion,
	}); err != nil {
		return RootHeader{}, nil, storage.ObjectVersion{}, err
	}

	var header RootHeader
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return RootHeader{}, nil, storage.ObjectVersion{}, fmt.Errorf("repo: parse root header: %w", err)
	}

	for _, k := range headerKeys {
		delete(top, k)
	}
	body, err := json.Marshal(top)
	if err != nil {
		return RootHeader{}, nil, storage.ObjectVersion{}, fmt.Errorf("repo: re-marshal body: %w", err)
	}
	return header, body, obj.Metadata.Version, nil
}

// CASRoot performs a PutIfVersionMatches against key with the given
// bytes. Storage-layer errors are wrapped so callers can errors.Is
// against storage sentinels.
func CASRoot(ctx context.Context, s storage.ObjectStore, key string, body []byte, prev storage.ObjectVersion) (storage.ObjectVersion, error) {
	v, err := s.PutIfVersionMatches(ctx, key, prev, strings.NewReader(string(body)), nil)
	if err != nil {
		return storage.ObjectVersion{}, fmt.Errorf("repo: cas root: %w", err)
	}
	return v, nil
}

// WrapHeaderInBody splices the M1-owned header keys into a body JSON
// object and returns the full root manifest bytes. Errors if the body
// already contains any of the reserved header keys (the caller would
// be claiming to own a field M1 owns).
func WrapHeaderInBody(h RootHeader, body json.RawMessage) ([]byte, error) {
	var top map[string]json.RawMessage
	if len(body) == 0 {
		top = map[string]json.RawMessage{}
	} else {
		if err := json.Unmarshal(body, &top); err != nil {
			return nil, fmt.Errorf("repo: body must be a JSON object: %w", err)
		}
		if top == nil {
			return nil, fmt.Errorf("repo: body must be a JSON object, got null")
		}
	}
	for _, k := range headerKeys {
		if _, ok := top[k]; ok {
			return nil, fmt.Errorf("repo: body must not contain reserved header key %q", k)
		}
	}
	headerJSON, err := json.Marshal(h)
	if err != nil {
		return nil, fmt.Errorf("repo: marshal header: %w", err)
	}
	var headerMap map[string]json.RawMessage
	if err := json.Unmarshal(headerJSON, &headerMap); err != nil {
		return nil, fmt.Errorf("repo: re-parse header: %w", err)
	}
	for k, v := range headerMap {
		top[k] = v
	}
	return json.Marshal(top)
}

func pickHeader(top map[string]json.RawMessage) (json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	for _, k := range headerKeys {
		v, ok := top[k]
		if !ok {
			continue
		}
		out[k] = v
	}
	return json.Marshal(out)
}

// compatHeader is the minimal subset of RootHeader needed to run the
// §43.7 schema gate. Parsing this first lets ReadRoot return
// repo.ErrUnsupportedSchema rather than a generic parse error when a
// future schema changes the type of any other header field.
type compatHeader struct {
	SchemaVersion    int    `json:"schema_version"`
	MinReaderVersion string `json:"min_reader_version"`
}
