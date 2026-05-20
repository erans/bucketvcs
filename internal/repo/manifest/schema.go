package manifest

import (
	"fmt"

	"github.com/bucketvcs/bucketvcs/internal/repo/repoerrs"
	"golang.org/x/mod/semver"
)

const (
	// CurrentSchemaVersion is the schema_version this build emits and
	// the highest schema_version this build accepts. Per §43.7 the gate
	// is asymmetric: future versions fail closed. M12 bumped 1 → 2 to
	// introduce sharded refs (Body.RefShards).
	CurrentSchemaVersion = 2

	// SupportedReaderVersion is the minimum reader version this build
	// satisfies. Manifests with min_reader_version > this value are
	// rejected at read time. Plain semver string (no leading "v"); the
	// gate adds the "v" prefix expected by golang.org/x/mod/semver.
	SupportedReaderVersion = "0.1.0"
)

// SchemaGate enforces the §43.7 fail-closed compatibility check. Returns
// repo.ErrUnsupportedSchema (wrapped with detail) if the header would
// require a newer reader; nil if this build can read the manifest.
func SchemaGate(h RootHeader) error {
	if h.SchemaVersion < 1 || h.SchemaVersion > CurrentSchemaVersion {
		return fmt.Errorf("%w: schema_version=%d (supported max=%d)",
			repoerrs.ErrUnsupportedSchema, h.SchemaVersion, CurrentSchemaVersion)
	}
	if h.MinReaderVersion == "" {
		return nil
	}
	mr := vPrefix(h.MinReaderVersion)
	supported := vPrefix(SupportedReaderVersion)
	if !semver.IsValid(mr) {
		return fmt.Errorf("%w: min_reader_version=%q is not valid semver",
			repoerrs.ErrUnsupportedSchema, h.MinReaderVersion)
	}
	if semver.Compare(mr, supported) > 0 {
		return fmt.Errorf("%w: min_reader_version=%q exceeds supported=%q",
			repoerrs.ErrUnsupportedSchema, h.MinReaderVersion, SupportedReaderVersion)
	}
	return nil
}

func vPrefix(s string) string {
	if len(s) > 0 && s[0] == 'v' {
		return s
	}
	return "v" + s
}
