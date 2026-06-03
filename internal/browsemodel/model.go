// Package browsemodel holds the view-model DTOs and sentinel errors shared by
// internal/gitbrowse (the storage-touching producer) and internal/web (the
// consumer). It deliberately imports nothing beyond the standard library so the
// web layer can depend on it without pulling in the storage/mirror packages,
// preserving the Phase 1 decoupling rule.
package browsemodel

import "errors"

// Sentinel errors crossing the ContentStore boundary.
var (
	// ErrNotFound means a repo, ref, path, or object does not exist. The web
	// layer maps it to HTTP 404.
	ErrNotFound = errors.New("browsemodel: not found")
	// ErrWarming means the on-disk mirror is still materializing and exceeded
	// the browse timeout. The web layer maps it to HTTP 503.
	ErrWarming = errors.New("browsemodel: repository warming up")
)

// RefInfo is a single branch or tag with its resolved commit OID.
type RefInfo struct {
	Name string // short name, e.g. "main" or "feature/foo" (no refs/heads/ prefix)
	OID  string // 40-hex commit OID
}

// Refs is the set of branches and tags plus the repo's default branch name.
type Refs struct {
	Default  string // short default-branch name, e.g. "main"; "" for an empty repo
	Branches []RefInfo
	Tags     []RefInfo
}

// Resolved is the outcome of splitting a URL remainder into a ref (or raw OID)
// and a path. Ref is the display name echoed in links/switcher; OID is the
// stable 40-hex handle used for content reads.
type Resolved struct {
	Ref  string // display ref name, or "" when the URL used a raw OID
	OID  string // 40-hex commit OID the ref/OID resolved to
	Path string // repo-relative path after the ref segment (no leading slash)
}

// TreeEntry is one row in a directory listing.
type TreeEntry struct {
	Name string // basename
	Path string // full repo-relative path
	Mode string // git mode, e.g. "100644"
	Type string // "tree" | "blob" | "commit" (submodule/gitlink)
	Size int64  // blob size in bytes; 0 for trees/commits
	OID  string // 40-hex object OID
}

// Blob is a file's content plus rendering hints.
type Blob struct {
	Path     string
	OID      string
	Size     int64
	Binary   bool   // contains a NUL byte in the first 8 KiB
	TooLarge bool   // size exceeds the hard read cap; Bytes is nil
	Bytes    []byte // content bytes; nil only when TooLarge (binary blobs still carry bytes)
}

// CommitMeta is the summary form used in logs and as the header of a commit view.
type CommitMeta struct {
	OID         string
	ShortOID    string // first 12 hex chars
	Summary     string // first line of the message
	AuthorName  string
	AuthorEmail string
	AuthorTime  int64 // unix seconds
}

// DiffLine is one line within a hunk. Kind is ' ' (context), '+' (added), or
// '-' (removed).
type DiffLine struct {
	Kind byte
	Text string // line content without the leading +/-/space
}

// Hunk is a contiguous @@ ... @@ block.
type Hunk struct {
	Header string // the literal "@@ -a,b +c,d @@ ..." line
	Lines  []DiffLine
}

// FileDiff is the diff for a single file within a commit.
type FileDiff struct {
	OldPath   string
	NewPath   string
	Status    string // "A"|"M"|"D"|"R"|"C"
	Binary    bool   // git reported a binary file instead of a textual diff
	TooLarge  bool   // exceeded the per-file line cap; Hunks is nil
	Additions int
	Deletions int
	Hunks     []Hunk
}

// CommitDetail is a single commit with metadata, message, parents, and diff.
type CommitDetail struct {
	Meta      CommitMeta
	Message   string
	Parents   []string
	Files     []FileDiff
	Truncated bool // diff exceeded the file cap; Files is partial
}
