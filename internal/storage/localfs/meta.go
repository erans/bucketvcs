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
// sidecar's Version field does not match the version this binary
// understands. It is distinct from generic parse failures because
// callers MUST fail closed rather than self-heal: an older binary
// that overwrites a future-schema sidecar with a current-schema
// recompute would silently downgrade the on-disk format.
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
	if s.Version != sidecarSchemaVersion {
		return sidecar{}, fmt.Errorf("%w: got %d, want %d", ErrUnsupportedSidecarSchema, s.Version, sidecarSchemaVersion)
	}
	return s, nil
}
