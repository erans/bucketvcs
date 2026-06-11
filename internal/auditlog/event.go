// Package auditlog is the read-side counterpart of internal/shiplog.
// It decodes gzipped NDJSON activity records into typed Events and filters them.
// Each record is a JSON object with ts (RFC3339Nano), level, event (the slog
// message), and audit attrs flattened at root (tenant, repo, actor/user, …).
package auditlog

import "time"

// Event is a decoded audit log record.
type Event struct {
	Ts     time.Time
	Level  string
	Event  string
	Tenant string
	Repo   string
	Actor  string
	// Attrs holds all fields not lifted into the typed fields above.
	// actor and user are retained here for the details view even though
	// Actor is resolved from them.
	Attrs map[string]any
}
