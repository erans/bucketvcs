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
// and lines over maxLineBytes are skipped and counted rather than aborting the
// batch. Empty lines are silently ignored and not counted as malformed. An
// object whose decompressed size exceeds maxObjectDecompressed returns an
// error (a partial decode is never silently returned as complete).
func DecodeGz(r io.Reader) ([]Event, int, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, 0, fmt.Errorf("auditlog: gzip open: %w", err)
	}
	defer gz.Close()

	// Read one byte past the cap so a stream that exactly fills the limit is
	// distinguishable from one that was cut off: consuming more than
	// maxObjectDecompressed means the object kept going and we'd be returning a
	// silently truncated decode.
	cr := &countingReader{r: gz}
	br := bufio.NewReaderSize(io.LimitReader(cr, maxObjectDecompressed+1), 64<<10)

	var events []Event
	skipped := 0
	var line []byte
	overLong := false

	// flush decodes the accumulated line (or counts it: over-long lines and
	// malformed JSON are skipped; empty lines are ignored) and resets state.
	flush := func() {
		// CRLF parity with the previous bufio.ScanLines implementation.
		if n := len(line); n > 0 && line[n-1] == '\r' {
			line = line[:n-1]
		}
		switch {
		case overLong:
			skipped++
		case len(line) > 0:
			var m map[string]any
			if err := json.Unmarshal(line, &m); err != nil {
				skipped++
			} else {
				events = append(events, eventFromMap(m))
			}
		}
		line = line[:0]
		overLong = false
	}

	for {
		frag, err := br.ReadSlice('\n')
		if n := len(frag); n > 0 && frag[n-1] == '\n' {
			frag = frag[:n-1]
		}
		if !overLong {
			if len(line)+len(frag) > maxLineBytes {
				// Keep consuming the rest of the line but discard it: one
				// oversized line must not fail the surrounding batch.
				overLong = true
				line = line[:0]
			} else {
				line = append(line, frag...)
			}
		}
		switch err {
		case nil:
			flush() // delimiter found: full line accumulated
		case bufio.ErrBufferFull:
			// mid-line: keep reading fragments
		case io.EOF:
			flush() // final line without trailing newline
			if cr.n > maxObjectDecompressed {
				return nil, skipped, fmt.Errorf("auditlog: object exceeds %d-byte decompressed limit", int64(maxObjectDecompressed))
			}
			return events, skipped, nil
		default:
			return nil, skipped, fmt.Errorf("auditlog: scan: %w", err)
		}
	}
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
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
