package auditlog_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auditlog"
)

func TestFilter_Match(t *testing.T) {
	base := time.Date(2026, 5, 22, 10, 0, 0, 0, time.UTC)
	e := auditlog.Event{
		Ts:     base,
		Level:  "INFO",
		Event:  "repo.created",
		Tenant: "acme",
		Repo:   "myrepo",
		Actor:  "alice",
		Attrs:  map[string]any{"extra": "val"},
	}

	tests := []struct {
		name  string
		f     auditlog.Filter
		match bool
	}{
		{
			name:  "zero filter matches everything",
			f:     auditlog.Filter{},
			match: true,
		},
		{
			name:  "EventPrefix exact match",
			f:     auditlog.Filter{EventPrefix: "repo.created"},
			match: true,
		},
		{
			name:  "EventPrefix prefix match",
			f:     auditlog.Filter{EventPrefix: "repo."},
			match: true,
		},
		{
			name:  "EventPrefix no match",
			f:     auditlog.Filter{EventPrefix: "auth."},
			match: false,
		},
		{
			name:  "Tenant match",
			f:     auditlog.Filter{Tenant: "acme"},
			match: true,
		},
		{
			name:  "Tenant no match",
			f:     auditlog.Filter{Tenant: "other"},
			match: false,
		},
		{
			name:  "Repo match",
			f:     auditlog.Filter{Repo: "myrepo"},
			match: true,
		},
		{
			name:  "Repo no match",
			f:     auditlog.Filter{Repo: "other"},
			match: false,
		},
		{
			name:  "Actor match",
			f:     auditlog.Filter{Actor: "alice"},
			match: true,
		},
		{
			name:  "Actor no match",
			f:     auditlog.Filter{Actor: "bob"},
			match: false,
		},
		{
			name:  "Since inclusive boundary",
			f:     auditlog.Filter{Since: base},
			match: true,
		},
		{
			name:  "Since exclusive (event before since)",
			f:     auditlog.Filter{Since: base.Add(time.Second)},
			match: false,
		},
		{
			name:  "Until inclusive boundary",
			f:     auditlog.Filter{Until: base},
			match: true,
		},
		{
			name:  "Until exclusive (event after until)",
			f:     auditlog.Filter{Until: base.Add(-time.Second)},
			match: false,
		},
		{
			name:  "combined all match",
			f:     auditlog.Filter{EventPrefix: "repo.", Tenant: "acme", Repo: "myrepo", Actor: "alice", Since: base, Until: base},
			match: true,
		},
		{
			name:  "combined one field fails",
			f:     auditlog.Filter{EventPrefix: "repo.", Tenant: "acme", Repo: "myrepo", Actor: "bob"},
			match: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.f.Match(e)
			if got != tc.match {
				t.Errorf("Filter%+v .Match(e) = %v, want %v", tc.f, got, tc.match)
			}
		})
	}
}

func TestFilter_UserActorFallback_MatchesOnActor(t *testing.T) {
	// Confirm that an event resolved via user→actor fallback is matched by Actor filter.
	ts := "2026-05-22T10:00:00Z"
	line := `{"ts":"` + ts + `","level":"INFO","event":"auth.login","user":"bob"}`
	events, _, err := auditlog.DecodeGz(bytes.NewReader(gzLines(line)))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	f := auditlog.Filter{Actor: "bob"}
	if !f.Match(events[0]) {
		t.Errorf("Filter{Actor:bob} should match event with user=bob resolved as Actor")
	}
	f2 := auditlog.Filter{Actor: "alice"}
	if f2.Match(events[0]) {
		t.Errorf("Filter{Actor:alice} should NOT match event with Actor=bob")
	}
}
