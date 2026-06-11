package auditlog

import (
	"strings"
	"time"
)

// Filter selects Events that match all non-zero criteria.
// A zero-value Filter matches every event.
type Filter struct {
	// EventPrefix matches events whose Event field starts with this string.
	// Empty string matches any event.
	EventPrefix string
	// Tenant, Repo, Actor match exactly. Empty string matches any value.
	Tenant string
	Repo   string
	Actor  string
	// Since and Until are inclusive time bounds. Zero values are ignored.
	Since time.Time
	Until time.Time
}

// Match reports whether e satisfies all non-zero criteria in f.
func (f Filter) Match(e Event) bool {
	if f.EventPrefix != "" && !strings.HasPrefix(e.Event, f.EventPrefix) {
		return false
	}
	if f.Tenant != "" && e.Tenant != f.Tenant {
		return false
	}
	if f.Repo != "" && e.Repo != f.Repo {
		return false
	}
	if f.Actor != "" && e.Actor != f.Actor {
		return false
	}
	if !f.Since.IsZero() && e.Ts.Before(f.Since) {
		return false
	}
	if !f.Until.IsZero() && e.Ts.After(f.Until) {
		return false
	}
	return true
}
