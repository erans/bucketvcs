package auditlog

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// maxObjectDecompressed is the maximum total decompressed size we will read.
// A single gzip activity segment should never exceed this.
const maxObjectDecompressed = 64 << 20 // 64 MiB

// maxLineBytes is the maximum size of a single NDJSON line.
const maxLineBytes = 4 << 20 // 4 MiB

// DecodeGz decompresses r as gzip and decodes the NDJSON lines into Events.
// It returns the decoded events, the number of malformed (skipped) lines, and
// any error. A gzip-level error is returned immediately; malformed JSON lines
// are skipped and counted rather than aborting the batch. Empty lines are
// silently ignored and not counted as malformed.
func DecodeGz(r io.Reader) ([]Event, int, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, 0, fmt.Errorf("auditlog: gzip open: %w", err)
	}
	defer gz.Close()

	scanner := bufio.NewScanner(io.LimitReader(gz, maxObjectDecompressed))
	buf := make([]byte, maxLineBytes)
	scanner.Buffer(buf, maxLineBytes)

	var events []Event
	skipped := 0

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue // silently skip empty lines
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			skipped++
			continue
		}
		events = append(events, eventFromMap(m))
	}
	if err := scanner.Err(); err != nil {
		return nil, skipped, fmt.Errorf("auditlog: scan: %w", err)
	}
	return events, skipped, nil
}

// eventFromMap lifts well-known keys into typed Event fields. actor wins over
// user regardless of JSON key order. Both actor and user are kept in Attrs for
// the details view.
func eventFromMap(m map[string]any) Event {
	e := Event{}
	e.Ts = parseTime(asString(m["ts"]))
	e.Level = asString(m["level"])
	e.Event = asString(m["event"])
	e.Tenant = asString(m["tenant"])
	e.Repo = asString(m["repo"])

	// Resolve actor: actor wins; fall back to user. Done AFTER reading the full
	// map so key order in the JSON object is irrelevant.
	e.Actor = asString(m["actor"])
	if e.Actor == "" {
		e.Actor = asString(m["user"])
	}

	// Everything except the lifted structural keys goes into Attrs.
	// actor/user are intentionally retained in Attrs for the details view.
	lifted := map[string]bool{
		"ts": true, "level": true, "event": true, "tenant": true, "repo": true,
	}
	attrs := make(map[string]any, len(m))
	for k, v := range m {
		if !lifted[k] {
			attrs[k] = v
		}
	}
	e.Attrs = attrs
	return e
}

// asString returns the string value of v, or "" if v is not a string or is nil.
func asString(v any) string {
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

// parseTime parses an RFC3339Nano timestamp. Returns zero time on empty input
// or parse failure.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
