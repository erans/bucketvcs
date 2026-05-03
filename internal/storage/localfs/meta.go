package localfs

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// sidecar is the JSON-encoded metadata file written next to every
// localfs object: <root>/objects/<key>.meta. The Version field gates
// schema migrations; an unknown version causes parseSidecar to fail
// rather than guess.
type sidecar struct {
	Version     int       `json:"version"`
	Sha256      string    `json:"sha256"`
	Size        int64     `json:"size"`
	ContentType string    `json:"content_type"`
	ModifiedAt  time.Time `json:"modified_at"`
}

const sidecarSchemaVersion = 1

// ErrUnsupportedSidecarSchema is returned by parseSidecar when the
// sidecar's Version field is HIGHER than the version this binary
// understands (i.e. a forward-compatibility wall). It is distinct from
// generic parse failures because callers MUST fail closed rather than
// self-heal: an older binary that overwrites a future-schema sidecar
// with a current-schema recompute would silently downgrade the on-disk
// format.
//
// Missing, zero, negative, or otherwise corrupt version values are
// NOT this error — they are returned as plain errors so the headLocked
// self-heal path can recompute the sidecar from content. Only strict
// version-greater-than-current triggers fail-closed semantics.
var ErrUnsupportedSidecarSchema = errors.New("localfs: unsupported sidecar schema version")

func newSidecar(sha256 string, size int64, contentType string, modifiedAt time.Time) sidecar {
	return sidecar{
		Version:     sidecarSchemaVersion,
		Sha256:      sha256,
		Size:        size,
		ContentType: contentType,
		ModifiedAt:  modifiedAt.UTC(),
	}
}

func encodeSidecar(s sidecar) ([]byte, error) {
	return json.Marshal(s)
}

func parseSidecar(data []byte) (sidecar, error) {
	var s sidecar
	if err := json.Unmarshal(data, &s); err != nil {
		return sidecar{}, fmt.Errorf("parseSidecar: %w", err)
	}
	if s.Version > sidecarSchemaVersion {
		// Future schema: fail closed so this binary does not silently
		// downgrade a sidecar a future binary wrote.
		return sidecar{}, fmt.Errorf("%w: got %d, want %d", ErrUnsupportedSidecarSchema, s.Version, sidecarSchemaVersion)
	}
	if s.Version != sidecarSchemaVersion {
		// Missing (default 0), negative, or otherwise unexpected
		// version. Treat as ordinary corruption so headLocked's
		// self-heal path runs and rebuilds a valid sidecar from
		// content.
		return sidecar{}, fmt.Errorf("parseSidecar: corrupt schema version %d", s.Version)
	}
	return s, nil
}
