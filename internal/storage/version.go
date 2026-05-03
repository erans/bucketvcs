package storage

// ObjectVersion is the normalized cross-provider version token. Core
// repository logic compares versions by value and never inspects Provider
// or Kind for routing decisions; those fields exist for diagnostics and
// for adapters that need to round-trip provider metadata.
//
// Token semantics are adapter-defined and opaque to callers: localfs uses
// hex-encoded sha256 of content, S3 uses the ETag, GCS uses the
// generation, and so on.
type ObjectVersion struct {
	Provider string
	Token    string
	Kind     VersionKind
}

// VersionKind is a hint about the provider-native form of an
// ObjectVersion's token. Callers must not switch on Kind for correctness;
// it is informational.
type VersionKind int

const (
	VersionUnknown VersionKind = iota
	VersionEtag
	VersionGeneration
	VersionVersionID
	VersionOpaque
)

// String returns a stable lowercase label for the kind, suitable for logs
// and error messages.
func (k VersionKind) String() string {
	switch k {
	case VersionEtag:
		return "etag"
	case VersionGeneration:
		return "generation"
	case VersionVersionID:
		return "version_id"
	case VersionOpaque:
		return "opaque"
	default:
		return "unknown"
	}
}
