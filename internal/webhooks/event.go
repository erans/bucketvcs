package webhooks

import (
	"fmt"
	"strings"
)

// Event is a bitmask identifying which kind of webhook fires.
type Event uint64

const (
	EventPush              Event = 1 << 0
	EventLFSUpload         Event = 1 << 1
	EventLFSLockCreated    Event = 1 << 2
	EventLFSLockReleased   Event = 1 << 3
	EventRepoCreated       Event = 1 << 4
	EventRepoDeleted       Event = 1 << 5
	EventRepoRenamed       Event = 1 << 6
	EventPolicyRefRejected Event = 1 << 7
)

const EventMaskAll Event = EventPush | EventLFSUpload | EventLFSLockCreated |
	EventLFSLockReleased | EventRepoCreated | EventRepoDeleted | EventRepoRenamed |
	EventPolicyRefRejected

// eventNames is the authoritative list of (event, canonical name) pairs.
// Ordering controls FormatEvents output and ParseEvents shortcut groups.
var eventNames = []struct {
	e    Event
	name string
}{
	{EventPush, "push"},
	{EventLFSUpload, "lfs.upload"},
	{EventLFSLockCreated, "lfs.lock.created"},
	{EventLFSLockReleased, "lfs.lock.released"},
	{EventRepoCreated, "repo.created"},
	{EventRepoDeleted, "repo.deleted"},
	{EventRepoRenamed, "repo.renamed"},
	{EventPolicyRefRejected, "policy.ref.rejected"},
}

var nameToEvent = func() map[string]Event {
	m := make(map[string]Event, len(eventNames))
	for _, p := range eventNames {
		m[p.name] = p.e
	}
	return m
}()

// String returns the canonical wire name of a single-bit Event. Returns
// "events(0xN)" for zero/multi-bit values.
func (e Event) String() string {
	for _, p := range eventNames {
		if p.e == e {
			return p.name
		}
	}
	return fmt.Sprintf("events(0x%x)", uint64(e))
}

// Has reports whether mask e includes the single-bit event x.
func (e Event) Has(x Event) bool {
	return e&x != 0
}

// FormatEvents returns a comma-separated canonical list, or "all" if every
// known event is set.
func FormatEvents(e Event) string {
	if e&EventMaskAll == EventMaskAll {
		return "all"
	}
	var names []string
	for _, p := range eventNames {
		if e.Has(p.e) {
			names = append(names, p.name)
		}
	}
	return strings.Join(names, ",")
}

// ParseEvents accepts:
//   - "all" → EventMaskAll
//   - "lfs.*" → all lfs.* events
//   - "repo.*" → all repo.* events
//   - comma-separated canonical names, whitespace tolerant
//
// Empty string and unknown names return an error.
func ParseEvents(s string) (Event, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty event list")
	}
	if s == "all" {
		return EventMaskAll, nil
	}
	var out Event
	for _, raw := range strings.Split(s, ",") {
		tok := strings.TrimSpace(raw)
		switch tok {
		case "lfs.*":
			out |= EventLFSUpload | EventLFSLockCreated | EventLFSLockReleased
		case "repo.*":
			out |= EventRepoCreated | EventRepoDeleted | EventRepoRenamed
		default:
			e, ok := nameToEvent[tok]
			if !ok {
				return 0, fmt.Errorf("unknown event %q", tok)
			}
			out |= e
		}
	}
	if out == 0 {
		return 0, fmt.Errorf("empty event list after parsing")
	}
	return out, nil
}

// --- Payload structs ---

// CommonEnvelope is the envelope wrapping every webhook payload. Every
// per-event Payload struct embeds it via composition; Enqueue (Task 3)
// fills in the envelope fields automatically.
type CommonEnvelope struct {
	DeliveryID string `json:"delivery_id"`
	Timestamp  int64  `json:"timestamp"`
	Event      string `json:"event"`
	Tenant     string `json:"tenant"`
	Repo       string `json:"repo"`
	Actor      string `json:"actor"`
}

// PushPayload is the body for EventPush.
type PushPayload struct {
	CommonEnvelope
	TxID            string         `json:"tx_id"`
	ManifestVersion int64          `json:"manifest_version"`
	StorageBackend  string         `json:"storage_backend"`
	RefUpdates      []RefUpdate    `json:"ref_updates"`
	CommitsSummary  CommitsSummary `json:"commits_summary"`
}

// RefUpdate is one entry in PushPayload.RefUpdates. old_oid == "0000..." means
// ref creation; new_oid == "0000..." means ref deletion (consumers infer
// branch/tag create/delete from these — see spec §4).
type RefUpdate struct {
	Refname string `json:"refname"`
	OldOID  string `json:"old_oid"`
	NewOID  string `json:"new_oid"`
}

// CommitsSummary is the minimal Tier 1 commit information. Per-commit walk
// deferred (spec §1.2).
type CommitsSummary struct {
	Count int    `json:"count"`
	Head  string `json:"head"`
}

// LFSUploadPayload is the body for EventLFSUpload.
type LFSUploadPayload struct {
	CommonEnvelope
	OID       string `json:"oid"`
	SizeBytes int64  `json:"size_bytes"`
}

// LFSLockPayload is the body for both EventLFSLockCreated and EventLFSLockReleased.
type LFSLockPayload struct {
	CommonEnvelope
	LockID string `json:"lock_id"`
	Path   string `json:"path"`
	Ref    string `json:"ref,omitempty"`
}

// RepoLifecyclePayload is the body for EventRepoCreated and EventRepoDeleted.
// The envelope alone suffices for these events.
type RepoLifecyclePayload struct {
	CommonEnvelope
}

// RepoRenamedPayload is the body for EventRepoRenamed.
type RepoRenamedPayload struct {
	CommonEnvelope
	OldName string `json:"old_name"`
	NewName string `json:"new_name"`
}

// PolicyRefRejectedPayload is the body for EventPolicyRefRejected.
type PolicyRefRejectedPayload struct {
	CommonEnvelope
	Refname        string `json:"refname"`
	MatchedPattern string `json:"matched_pattern"`
	Reason         string `json:"reason"`
	OldOID         string `json:"old_oid"`
	NewOID         string `json:"new_oid"`
	// MatchedPath is populated only for M16 path-rule rejections
	// (Reason == "blocked_path"). Omitted otherwise so receivers see
	// no key when empty.
	MatchedPath string `json:"matched_path,omitempty"`
}
