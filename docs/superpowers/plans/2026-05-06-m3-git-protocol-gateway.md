# M3 — Git Protocol Gateway Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship the bucketvcs HTTP smart-Git gateway: protocol v2 (`upload-pack`) + protocol v0 (`receive-pack`), pure-Go pkt-line + capability negotiation, shell-out to `git pack-objects`/`git index-pack` against a per-repo on-disk bare-repo mirror that is kept in sync with the bucket manifest. End state: `git clone http://localhost:PORT/<tenant>/<repo>.git` and `git push --mirror` work end-to-end against a localfs-backed repo, the differential harness covers clone- and push-equivalence vs upstream git, and an optional shared bearer token gates writes (or all).

**Architecture:** Track B (pure-Go protocol) on the gateway request layer; shell-out via `internal/gitcli` for pack production (`git pack-objects`) and pack ingestion (`git index-pack`). Mirror lifecycle B2: per-repo on-disk bare repo lazy-materialized via M2 exporter, advanced incrementally on push, rebuilt on stale-detection. In-process per-repo `sync.RWMutex` serializes pushes; `repo.Commit` CAS is the durable correctness primitive. v2-only for upload-pack; v0 for receive-pack (v2 doesn't redefine push).

**Tech Stack:** Go 1.25; module `github.com/bucketvcs/bucketvcs`; depends on `internal/storage` (M0), `internal/repo` + `internal/repo/manifest` + `internal/repo/keys` (M1), `internal/pack` + `internal/objindex` + `internal/commitgraph` + `internal/importer` + `internal/exporter` + `internal/gitcli` (M2); shells out to upstream `git` ≥ 2.40.

**Spec:** `docs/superpowers/specs/2026-05-06-m3-git-protocol-gateway-design.md`.

**Conventions:**
- Each task is one focused unit. Steps within a task are 2-5 minutes each.
- Every task ends with a commit. Use the `M3 ...` prefix consistently.
- Follow the M1+ review protocol from `m1_review_protocol.md`: superpowers code-reviewer per task, then roborev-refine on max reasoning until pass or diminishing returns.
- The protocol-format details required by tasks 1-5, 14-17 are documented in:
  - Pkt-line format: `Documentation/technical/protocol-common.txt` (https://git-scm.com/docs/protocol-common)
  - Protocol v2: `Documentation/technical/protocol-v2.txt` (https://git-scm.com/docs/protocol-v2)
  - HTTP protocol: `Documentation/technical/http-protocol.txt` (https://git-scm.com/docs/http-protocol)
  - Pack protocol (v0): `Documentation/technical/pack-protocol.txt` (https://git-scm.com/docs/pack-protocol)
  - Send-pack/receive-pack capabilities: `Documentation/technical/protocol-capabilities.txt` (https://git-scm.com/docs/protocol-capabilities)
- Citations like `[protocol-v2 §ls-refs]` refer to those documents.

**Reconciling spec → existing M2 surface:** M3 reuses the entire M2 stack as-is. The push write path calls into `internal/importer`'s pack→.bvom→.bvcg→upload→`repo.Commit` flow; Task 11 extracts a public `BuildAndCommit` entry point so the gateway doesn't depend on private internals. M3 adds **no new packages under `internal/repo/`** and does not touch `manifest.Body` schema (its golden file is unchanged).

**Package layout (new in M3):**

| Package | Files | Responsibility |
|---|---|---|
| `internal/pktline` | `pktline.go`, `sideband.go`, `*_test.go` | pkt-line frame I/O + side-band wrapper |
| `internal/v2proto` | `caps.go`, `lsrefs.go`, `fetch.go`, `*_test.go` | Protocol v2 dispatch (capability advert, ls-refs, fetch) |
| `internal/mirror` | `mirror.go`, `ingest.go`, `lock.go`, `*_test.go` | Per-repo on-disk bare-repo cache, manifest-version sync, per-repo mutex |
| `internal/gateway` | `server.go`, `routes.go`, `auth.go`, `upload_pack.go`, `receive_pack.go`, `*_test.go` | HTTP handlers wiring everything together |
| `cmd/bucketvcs` | `serve.go` (new) | `bucketvcs serve` subcommand |
| `internal/diffharness` | `clone_oracle_test.go`, `push_oracle_test.go` (new) | Clone- and push-equivalence oracles |
| `internal/diffharness/fixtures` | 5 new builders in `synthetic.go` (or new file) | New M3 fixtures |

**Existing packages M3 extends:**

- `internal/gitcli/gitcli.go` — adds `PackObjectsForFetch`, `IndexPackStrict`, `FsckConnectivityOnly`, `UpdateRefDelete`, `UpdateRefCAS`, `RevListNotAll`.
- `internal/importer/importer.go` — exports a public `BuildAndCommit(ctx, store, tenantID, repoID, packPath, refUpdates, current *manifest.Body) (newBody *manifest.Body, err error)` entry point. The push handler calls this; the existing `Import` is refactored to call it too (no behavior change).
- `cmd/bucketvcs/main.go` — adds `serve` to the subcommand router.

---

## Task 1: pktline package — Reader and Writer

**Files:**
- Create: `internal/pktline/pktline.go`
- Create: `internal/pktline/pktline_test.go`

`internal/pktline.Reader` and `Writer` operate on `io.Reader`/`io.Writer`. Each frame is `XXXX<payload>` where `XXXX` is a 4-byte ASCII hex length that includes the 4 length bytes themselves. Special markers: `0000` = flush, `0001` = delim (v2), `0002` = response-end (v2). Max payload `MaxPayload = 65516` (so total frame ≤ 65520). Reader rejects oversized frames and malformed lengths. [protocol-common §pkt-line Format].

- [ ] **Step 1: Write the failing test**

Create `internal/pktline/pktline_test.go`:

```go
package pktline

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReader_BasicFrame(t *testing.T) {
	r := NewReader(strings.NewReader("0009abcd\n"))
	tok, err := r.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if tok.Type != Data {
		t.Fatalf("Type: got %v, want Data", tok.Type)
	}
	if string(tok.Payload) != "abcd\n" {
		t.Fatalf("Payload: got %q, want %q", tok.Payload, "abcd\n")
	}
}

func TestReader_FlushDelimResponseEnd(t *testing.T) {
	r := NewReader(strings.NewReader("000000010002"))
	for _, want := range []TokenType{Flush, Delim, ResponseEnd} {
		tok, err := r.Read()
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if tok.Type != want {
			t.Fatalf("Type: got %v, want %v", tok.Type, want)
		}
		if len(tok.Payload) != 0 {
			t.Fatalf("Payload non-empty for marker: %q", tok.Payload)
		}
	}
}

func TestReader_EOFAfterAllFramesConsumed(t *testing.T) {
	r := NewReader(strings.NewReader("0000"))
	if _, err := r.Read(); err != nil {
		t.Fatalf("Read flush: %v", err)
	}
	if _, err := r.Read(); !errors.Is(err, io.EOF) {
		t.Fatalf("Read after end: got %v, want io.EOF", err)
	}
}

func TestReader_MalformedHexLength(t *testing.T) {
	r := NewReader(strings.NewReader("zzzzhello"))
	if _, err := r.Read(); err == nil {
		t.Fatalf("Read: expected error on malformed hex length")
	}
}

func TestReader_OversizedRejected(t *testing.T) {
	r := NewReader(strings.NewReader("ffff"))
	if _, err := r.Read(); err == nil {
		t.Fatalf("Read: expected error on oversized frame")
	}
}

func TestReader_LengthTooSmall(t *testing.T) {
	r := NewReader(strings.NewReader("0003"))
	if _, err := r.Read(); err == nil {
		t.Fatalf("Read: expected error on length < 4 (and not 0/1/2)")
	}
}

func TestReader_UnexpectedEOFInPayload(t *testing.T) {
	r := NewReader(strings.NewReader("0009ab"))
	_, err := r.Read()
	if err == nil {
		t.Fatalf("Read: expected error on truncated payload")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Read: got %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestWriter_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.WritePacket([]byte("hello\n")); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}
	if err := w.WriteDelim(); err != nil {
		t.Fatalf("WriteDelim: %v", err)
	}
	if err := w.WriteFlush(); err != nil {
		t.Fatalf("WriteFlush: %v", err)
	}

	r := NewReader(&buf)
	tok, err := r.Read()
	if err != nil || tok.Type != Data || string(tok.Payload) != "hello\n" {
		t.Fatalf("Read 1: %+v err=%v", tok, err)
	}
	tok, err = r.Read()
	if err != nil || tok.Type != Delim {
		t.Fatalf("Read 2 (delim): %+v err=%v", tok, err)
	}
	tok, err = r.Read()
	if err != nil || tok.Type != Flush {
		t.Fatalf("Read 3 (flush): %+v err=%v", tok, err)
	}
}

func TestWriter_RejectsOversizedPayload(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	big := bytes.Repeat([]byte{'x'}, MaxPayload+1)
	if err := w.WritePacket(big); err == nil {
		t.Fatalf("WritePacket: expected error on payload > MaxPayload")
	}
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/pktline/...`
Expected: FAIL with build errors (`undefined: NewReader`, etc.).

- [ ] **Step 3: Write the implementation**

Create `internal/pktline/pktline.go`:

```go
// Package pktline implements Git's pkt-line framing for the smart HTTP
// transport (and, in the future, SSH). Frames are length-prefixed with a
// 4-byte ASCII hex header that INCLUDES the 4 length bytes themselves.
//
// Special markers:
//   0000 = flush
//   0001 = delim   (protocol v2)
//   0002 = response-end (protocol v2)
//
// Reference: https://git-scm.com/docs/protocol-common
package pktline

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// MaxPayload is the largest allowed payload (excluding the 4-byte length
// header). Total wire frame size is therefore at most MaxPayload + 4 = 65520.
const MaxPayload = 65516

// TokenType classifies a pkt-line frame.
type TokenType int

const (
	Data TokenType = iota
	Flush
	Delim
	ResponseEnd
)

func (t TokenType) String() string {
	switch t {
	case Data:
		return "Data"
	case Flush:
		return "Flush"
	case Delim:
		return "Delim"
	case ResponseEnd:
		return "ResponseEnd"
	default:
		return fmt.Sprintf("TokenType(%d)", int(t))
	}
}

// Token is one decoded frame. For non-Data tokens, Payload is empty.
type Token struct {
	Type    TokenType
	Payload []byte
}

// Reader reads pkt-line frames from an underlying io.Reader.
type Reader struct {
	r   io.Reader
	hdr [4]byte
	buf []byte // payload buffer, grown as needed up to MaxPayload
}

func NewReader(r io.Reader) *Reader {
	return &Reader{r: r}
}

// Read returns the next pkt-line token. At true end of stream after a
// previously-returned Flush (or any clean boundary), Read returns io.EOF.
// On a truncated or malformed frame, an error is returned.
func (r *Reader) Read() (Token, error) {
	n, err := io.ReadFull(r.r, r.hdr[:])
	if err == io.EOF && n == 0 {
		return Token{}, io.EOF
	}
	if err != nil {
		return Token{}, fmt.Errorf("pktline: read length: %w", err)
	}
	var lenBytes [2]byte
	if _, err := hex.Decode(lenBytes[:], r.hdr[:]); err != nil {
		return Token{}, fmt.Errorf("pktline: malformed hex length %q: %w", string(r.hdr[:]), err)
	}
	frameLen := int(lenBytes[0])<<8 | int(lenBytes[1])
	switch frameLen {
	case 0:
		return Token{Type: Flush}, nil
	case 1:
		return Token{Type: Delim}, nil
	case 2:
		return Token{Type: ResponseEnd}, nil
	}
	if frameLen < 4 {
		return Token{}, fmt.Errorf("pktline: invalid frame length %d", frameLen)
	}
	if frameLen > MaxPayload+4 {
		return Token{}, fmt.Errorf("pktline: frame length %d exceeds max %d", frameLen, MaxPayload+4)
	}
	payloadLen := frameLen - 4
	if cap(r.buf) < payloadLen {
		r.buf = make([]byte, payloadLen)
	} else {
		r.buf = r.buf[:payloadLen]
	}
	if _, err := io.ReadFull(r.r, r.buf); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return Token{}, fmt.Errorf("pktline: read payload: %w", err)
	}
	return Token{Type: Data, Payload: r.buf}, nil
}

// Writer emits pkt-line frames to an underlying io.Writer.
type Writer struct {
	w   io.Writer
	hdr [4]byte
}

func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w}
}

// WritePacket writes one Data frame whose payload is p. Returns an error if
// len(p) exceeds MaxPayload.
func (w *Writer) WritePacket(p []byte) error {
	if len(p) > MaxPayload {
		return fmt.Errorf("pktline: payload size %d exceeds max %d", len(p), MaxPayload)
	}
	frameLen := len(p) + 4
	w.hdr[0] = hexNibble(byte(frameLen >> 12))
	w.hdr[1] = hexNibble(byte(frameLen >> 8 & 0xf))
	w.hdr[2] = hexNibble(byte(frameLen >> 4 & 0xf))
	w.hdr[3] = hexNibble(byte(frameLen & 0xf))
	if _, err := w.w.Write(w.hdr[:]); err != nil {
		return fmt.Errorf("pktline: write header: %w", err)
	}
	if len(p) > 0 {
		if _, err := w.w.Write(p); err != nil {
			return fmt.Errorf("pktline: write payload: %w", err)
		}
	}
	return nil
}

// WriteFlush writes a flush-pkt (0000).
func (w *Writer) WriteFlush() error {
	_, err := w.w.Write([]byte("0000"))
	return err
}

// WriteDelim writes a delim-pkt (0001) used by protocol v2.
func (w *Writer) WriteDelim() error {
	_, err := w.w.Write([]byte("0001"))
	return err
}

// WriteResponseEnd writes a response-end-pkt (0002) used by protocol v2.
func (w *Writer) WriteResponseEnd() error {
	_, err := w.w.Write([]byte("0002"))
	return err
}

// WriteString is a convenience for WritePacket([]byte(s)).
func (w *Writer) WriteString(s string) error { return w.WritePacket([]byte(s)) }

func hexNibble(n byte) byte {
	if n < 10 {
		return '0' + n
	}
	return 'a' + (n - 10)
}
```

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./internal/pktline/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/pktline/pktline.go internal/pktline/pktline_test.go
git commit -m "M3 pktline: Reader and Writer with flush/delim/response-end markers"
```

---

## Task 2: pktline side-band wrapper

**Files:**
- Create: `internal/pktline/sideband.go`
- Create: `internal/pktline/sideband_test.go`

Side-band-64k multiplexes three logical channels over a single pkt-line stream: band 1 = data, band 2 = progress, band 3 = fatal error. Each frame's first payload byte is the band id; the rest is the channel payload. Max channel payload per frame is `MaxPayload - 1 = 65515` bytes. [protocol-capabilities §side-band-64k].

- [ ] **Step 1: Write the failing test**

Create `internal/pktline/sideband_test.go`:

```go
package pktline

import (
	"bytes"
	"strings"
	"testing"
)

func TestSidebandWriter_BandsAndChunking(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	sb := NewSidebandWriter(w)

	if _, err := sb.WriteData([]byte("hello")); err != nil {
		t.Fatalf("WriteData: %v", err)
	}
	if _, err := sb.WriteProgress([]byte("counting...\n")); err != nil {
		t.Fatalf("WriteProgress: %v", err)
	}
	if _, err := sb.WriteFatal([]byte("boom")); err != nil {
		t.Fatalf("WriteFatal: %v", err)
	}

	r := NewReader(&buf)
	tok, _ := r.Read()
	if tok.Type != Data || tok.Payload[0] != BandData || string(tok.Payload[1:]) != "hello" {
		t.Fatalf("frame 1: %+v", tok)
	}
	tok, _ = r.Read()
	if tok.Payload[0] != BandProgress || string(tok.Payload[1:]) != "counting...\n" {
		t.Fatalf("frame 2: %+v", tok)
	}
	tok, _ = r.Read()
	if tok.Payload[0] != BandFatal || string(tok.Payload[1:]) != "boom" {
		t.Fatalf("frame 3: %+v", tok)
	}
}

func TestSidebandWriter_LargePayloadChunks(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	sb := NewSidebandWriter(w)
	big := bytes.Repeat([]byte("x"), MaxPayload*2+100)
	n, err := sb.WriteData(big)
	if err != nil {
		t.Fatalf("WriteData: %v", err)
	}
	if n != len(big) {
		t.Fatalf("WriteData: n=%d, want %d", n, len(big))
	}

	r := NewReader(&buf)
	var got []byte
	for {
		tok, err := r.Read()
		if err != nil {
			break
		}
		if tok.Type != Data || tok.Payload[0] != BandData {
			t.Fatalf("unexpected frame: %+v", tok)
		}
		got = append(got, tok.Payload[1:]...)
	}
	if !bytes.Equal(got, big) {
		t.Fatalf("reassembled payload mismatch: len=%d want=%d", len(got), len(big))
	}
}

func TestSidebandWriter_RejectsBadBand(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	sb := NewSidebandWriter(w)
	if _, err := sb.write(0, []byte("x")); err == nil {
		t.Fatalf("write band 0: expected error")
	}
	if _, err := sb.write(4, []byte("x")); err == nil {
		t.Fatalf("write band 4: expected error")
	}
}

// Compile-time interface check.
var _ = strings.NewReader
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/pktline/...`
Expected: FAIL with `undefined: NewSidebandWriter`, etc.

- [ ] **Step 3: Write the implementation**

Create `internal/pktline/sideband.go`:

```go
package pktline

import "fmt"

// Side-band channel ids (Git side-band-64k).
const (
	BandData     byte = 1
	BandProgress byte = 2
	BandFatal    byte = 3
)

// SidebandPayloadMax is the largest application-level payload writable in a
// single side-band frame: one byte for the band id, the rest for data.
const SidebandPayloadMax = MaxPayload - 1

// SidebandWriter wraps a *Writer to emit side-band-multiplexed frames.
// Calls to WriteData/WriteProgress/WriteFatal automatically chunk payloads
// larger than SidebandPayloadMax across multiple frames.
type SidebandWriter struct {
	w   *Writer
	buf []byte
}

func NewSidebandWriter(w *Writer) *SidebandWriter {
	return &SidebandWriter{w: w, buf: make([]byte, MaxPayload)}
}

// WriteData writes p on band 1 (BandData), chunking as needed. Returns the
// number of payload bytes written or an error from the underlying writer.
func (s *SidebandWriter) WriteData(p []byte) (int, error) { return s.write(BandData, p) }

// WriteProgress writes p on band 2 (BandProgress).
func (s *SidebandWriter) WriteProgress(p []byte) (int, error) { return s.write(BandProgress, p) }

// WriteFatal writes p on band 3 (BandFatal).
func (s *SidebandWriter) WriteFatal(p []byte) (int, error) { return s.write(BandFatal, p) }

func (s *SidebandWriter) write(band byte, p []byte) (int, error) {
	if band < 1 || band > 3 {
		return 0, fmt.Errorf("pktline: invalid sideband %d", band)
	}
	written := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > SidebandPayloadMax {
			chunk = chunk[:SidebandPayloadMax]
		}
		s.buf = s.buf[:0]
		s.buf = append(s.buf, band)
		s.buf = append(s.buf, chunk...)
		if err := s.w.WritePacket(s.buf); err != nil {
			return written, err
		}
		written += len(chunk)
		p = p[len(chunk):]
	}
	return written, nil
}
```

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./internal/pktline/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/pktline/sideband.go internal/pktline/sideband_test.go
git commit -m "M3 pktline: side-band-64k writer with auto-chunking"
```

---

## Task 3: v2proto capability advertisement

**Files:**
- Create: `internal/v2proto/caps.go`
- Create: `internal/v2proto/caps_test.go`

The protocol-v2 capability advertisement is what the server emits on `GET /info/refs?service=git-upload-pack` when the client sent `Git-Protocol: version=2`. M3 ships exactly five lines after the smart-HTTP header: `version 2`, `agent=bucketvcs/<version>`, `ls-refs=unborn`, `fetch=shallow`, `object-format=sha1`. We do not advertise `filter`, `bundle-uri`, `wait-for-done`, or `sideband-all`. [protocol-v2 §Capability Advertisement].

- [ ] **Step 1: Write the failing test**

Create `internal/v2proto/caps_test.go`:

```go
package v2proto

import (
	"bytes"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/pktline"
)

func TestWriteV2Advertisement_ContainsExpectedLines(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteV2Advertisement(&buf, "git-upload-pack", "0.1"); err != nil {
		t.Fatalf("WriteV2Advertisement: %v", err)
	}
	tokens := drainTokens(t, &buf)

	wantLines := []string{
		"# service=git-upload-pack\n",
		"",                       // flush
		"version 2\n",
		"agent=bucketvcs/0.1\n",
		"ls-refs=unborn\n",
		"fetch=shallow\n",
		"object-format=sha1\n",
		"",                       // flush
	}
	if len(tokens) != len(wantLines) {
		t.Fatalf("token count: got %d, want %d (%v)", len(tokens), len(wantLines), tokens)
	}
	for i, want := range wantLines {
		if want == "" {
			if tokens[i].Type != pktline.Flush {
				t.Errorf("token %d: type %v, want Flush", i, tokens[i].Type)
			}
			continue
		}
		if tokens[i].Type != pktline.Data {
			t.Errorf("token %d: type %v, want Data", i, tokens[i].Type)
		}
		if string(tokens[i].Payload) != want {
			t.Errorf("token %d payload: got %q, want %q", i, tokens[i].Payload, want)
		}
	}
}

func TestWriteV2Advertisement_AgentPrefixGuard(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteV2Advertisement(&buf, "git-upload-pack", "0.1\nls-refs=evil"); err == nil {
		t.Fatalf("WriteV2Advertisement: expected error on agent containing newline")
	}
}

func drainTokens(t *testing.T, r *bytes.Buffer) []pktline.Token {
	t.Helper()
	pr := pktline.NewReader(r)
	var out []pktline.Token
	for {
		tok, err := pr.Read()
		if err != nil {
			break
		}
		// Copy payload so the buffer reuse in pktline doesn't bite us.
		cp := append([]byte{}, tok.Payload...)
		out = append(out, pktline.Token{Type: tok.Type, Payload: cp})
	}
	return out
}

// keep imports alive
var _ = strings.NewReader
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/v2proto/...`
Expected: FAIL with `undefined: WriteV2Advertisement`.

- [ ] **Step 3: Write the implementation**

Create `internal/v2proto/caps.go`:

```go
// Package v2proto implements Git protocol v2 server-side dispatch:
// capability advertisement, ls-refs, and fetch.
//
// References:
//   https://git-scm.com/docs/protocol-v2
//   https://git-scm.com/docs/protocol-capabilities
package v2proto

import (
	"fmt"
	"io"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/pktline"
)

// AgentName is the bucketvcs identifier emitted in the agent= capability and
// in user-agent-shaped contexts. The version is filled in by callers.
const AgentName = "bucketvcs"

// WriteV2Advertisement writes the "smart" /info/refs body advertising
// protocol v2 with the M3 capability set. The service argument is the
// requested service ("git-upload-pack" or "git-receive-pack") and is echoed
// in the header line. The version is appended to "agent=bucketvcs/".
//
// Layout (see [protocol-v2 §Capability Advertisement]):
//
//   pkt-line: "# service=<service>\n"
//   flush
//   pkt-line: "version 2\n"
//   pkt-line: "agent=bucketvcs/<version>\n"
//   pkt-line: "ls-refs=unborn\n"
//   pkt-line: "fetch=shallow\n"
//   pkt-line: "object-format=sha1\n"
//   flush
func WriteV2Advertisement(w io.Writer, service, version string) error {
	if strings.ContainsAny(version, "\r\n\x00") {
		return fmt.Errorf("v2proto: agent version contains forbidden control characters")
	}
	if strings.ContainsAny(service, "\r\n\x00 ") {
		return fmt.Errorf("v2proto: service contains forbidden characters")
	}
	pw := pktline.NewWriter(w)
	if err := pw.WriteString("# service=" + service + "\n"); err != nil {
		return err
	}
	if err := pw.WriteFlush(); err != nil {
		return err
	}
	for _, line := range V2Capabilities(version) {
		if err := pw.WriteString(line + "\n"); err != nil {
			return err
		}
	}
	return pw.WriteFlush()
}

// V2Capabilities returns the M3 capability lines (without trailing LFs) in
// the exact order they should be advertised. Exposed for testing and reuse.
func V2Capabilities(version string) []string {
	return []string{
		"version 2",
		"agent=" + AgentName + "/" + version,
		"ls-refs=unborn",
		"fetch=shallow",
		"object-format=sha1",
	}
}
```

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./internal/v2proto/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/v2proto/caps.go internal/v2proto/caps_test.go
git commit -m "M3 v2proto: protocol v2 capability advertisement"
```

---

## Task 4: v2proto ls-refs command

**Files:**
- Create: `internal/v2proto/lsrefs.go`
- Create: `internal/v2proto/lsrefs_test.go`

`ls-refs` is the protocol-v2 replacement for v0/v1 ref advertisement. The server reads the command (`command=ls-refs`), reads optional arguments (`peel`, `symrefs`, `unborn`, `ref-prefix <prefix>` repeated), then emits one line per matching ref: `<oid> <refname>` plus optional `peeled:<oid>` and `symref-target:<target>` annotations. Output is terminated by a flush. Server input is parsed entirely from a `[]pktline.Token`. We answer from `manifest.Body.Refs` and `manifest.Body.DefaultBranch`. [protocol-v2 §ls-refs].

- [ ] **Step 1: Write the failing test**

Create `internal/v2proto/lsrefs_test.go`:

```go
package v2proto

import (
	"bytes"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/pktline"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

func tokensFromLines(lines ...string) []pktline.Token {
	var out []pktline.Token
	for _, l := range lines {
		switch l {
		case "FLUSH":
			out = append(out, pktline.Token{Type: pktline.Flush})
		case "DELIM":
			out = append(out, pktline.Token{Type: pktline.Delim})
		default:
			out = append(out, pktline.Token{Type: pktline.Data, Payload: []byte(l)})
		}
	}
	return out
}

func TestLsRefs_BasicAdvertisement(t *testing.T) {
	body := &manifest.Body{
		DefaultBranch: "main",
		Refs: map[string]string{
			"refs/heads/main":    "1111111111111111111111111111111111111111",
			"refs/heads/feature": "2222222222222222222222222222222222222222",
		},
	}
	args := tokensFromLines(
		"command=ls-refs\n",
		"DELIM",
		"FLUSH",
	)
	var buf bytes.Buffer
	if err := HandleLsRefs(args, body, &buf); err != nil {
		t.Fatalf("HandleLsRefs: %v", err)
	}
	got := drainPayloads(t, &buf)
	wantPrefix := []string{
		"2222222222222222222222222222222222222222 refs/heads/feature\n",
		"1111111111111111111111111111111111111111 refs/heads/main\n",
	}
	if !equalIgnoreOrder(got, wantPrefix) {
		t.Fatalf("output: got %v, want %v", got, wantPrefix)
	}
}

func TestLsRefs_SymrefAndRefPrefix(t *testing.T) {
	body := &manifest.Body{
		DefaultBranch: "main",
		Refs: map[string]string{
			"refs/heads/main": "1111111111111111111111111111111111111111",
			"refs/tags/v1":    "3333333333333333333333333333333333333333",
		},
	}
	args := tokensFromLines(
		"command=ls-refs\n",
		"DELIM",
		"symrefs\n",
		"ref-prefix refs/heads/\n",
		"FLUSH",
	)
	var buf bytes.Buffer
	if err := HandleLsRefs(args, body, &buf); err != nil {
		t.Fatalf("HandleLsRefs: %v", err)
	}
	got := drainPayloads(t, &buf)
	if len(got) != 1 {
		t.Fatalf("expected 1 line filtered to refs/heads/, got %v", got)
	}
	want := "1111111111111111111111111111111111111111 refs/heads/main symref-target:HEAD\n"
	if got[0] != want {
		t.Fatalf("output[0]: got %q, want %q", got[0], want)
	}
}

func TestLsRefs_UnbornHEAD(t *testing.T) {
	body := &manifest.Body{
		DefaultBranch: "main",
		Refs:          map[string]string{},
	}
	args := tokensFromLines(
		"command=ls-refs\n",
		"DELIM",
		"unborn\n",
		"symrefs\n",
		"FLUSH",
	)
	var buf bytes.Buffer
	if err := HandleLsRefs(args, body, &buf); err != nil {
		t.Fatalf("HandleLsRefs: %v", err)
	}
	got := drainPayloads(t, &buf)
	want := []string{"unborn HEAD symref-target:refs/heads/main\n"}
	if !equalIgnoreOrder(got, want) {
		t.Fatalf("output: got %v, want %v", got, want)
	}
}

func TestLsRefs_RejectsRefPrefixWithSpace(t *testing.T) {
	body := &manifest.Body{Refs: map[string]string{}}
	args := tokensFromLines(
		"command=ls-refs\n",
		"DELIM",
		"ref-prefix refs/heads/ extra\n",
		"FLUSH",
	)
	var buf bytes.Buffer
	if err := HandleLsRefs(args, body, &buf); err == nil {
		t.Fatalf("HandleLsRefs: expected error on multi-token ref-prefix")
	}
}

func drainPayloads(t *testing.T, r *bytes.Buffer) []string {
	t.Helper()
	pr := pktline.NewReader(r)
	var out []string
	for {
		tok, err := pr.Read()
		if err != nil {
			break
		}
		if tok.Type == pktline.Data {
			out = append(out, string(tok.Payload))
		}
	}
	return out
}

func equalIgnoreOrder(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	cnt := map[string]int{}
	for _, s := range a {
		cnt[s]++
	}
	for _, s := range b {
		cnt[s]--
		if cnt[s] < 0 {
			return false
		}
	}
	return true
}

var _ = strings.NewReader
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/v2proto/...`
Expected: FAIL with `undefined: HandleLsRefs`.

- [ ] **Step 3: Write the implementation**

Create `internal/v2proto/lsrefs.go`:

```go
package v2proto

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/pktline"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

// HandleLsRefs implements the protocol-v2 "ls-refs" command. Args is the full
// pkt-line token stream that was the request body (including the
// "command=ls-refs" line, the delim, the args, and the trailing flush). The
// response is written to w as pkt-line frames followed by a flush.
//
// Supported argument keywords (each on its own pkt-line frame):
//   peel              — emit "peeled:<oid>" annotations for tag refs (no-op
//                       in M3: peel info would require object-store reads
//                       that we'd rather defer; see "limitations" below).
//   symrefs           — emit "symref-target:<target>" annotations for the
//                       HEAD symref.
//   unborn            — include an "unborn" line for HEAD when the default
//                       branch ref does not yet exist.
//   ref-prefix <pfx>  — restrict output to refs whose name starts with <pfx>.
//                       Multiple ref-prefix args union (any-of).
//
// Limitations: M3 advertises ls-refs=unborn only. peel is parsed but does
// nothing because tag-peeling requires object-store reads (it can be added
// later by walking commitgraph). Tests for peel are not part of the M3
// matrix.
func HandleLsRefs(args []pktline.Token, body *manifest.Body, w io.Writer) error {
	var (
		wantSymrefs bool
		wantUnborn  bool
		prefixes    []string
	)
	if err := iterateArgs(args, "ls-refs", func(line string) error {
		switch {
		case line == "peel":
			// Parsed but not implemented in M3.
		case line == "symrefs":
			wantSymrefs = true
		case line == "unborn":
			wantUnborn = true
		case strings.HasPrefix(line, "ref-prefix "):
			prefix := strings.TrimPrefix(line, "ref-prefix ")
			if prefix == "" || strings.ContainsAny(prefix, " \t") {
				return fmt.Errorf("ls-refs: invalid ref-prefix %q", prefix)
			}
			prefixes = append(prefixes, prefix)
		default:
			return fmt.Errorf("ls-refs: unknown argument %q", line)
		}
		return nil
	}); err != nil {
		return err
	}

	pw := pktline.NewWriter(w)

	// HEAD line: in v2, HEAD is emitted only if the default branch is in the
	// advertised set, OR (with "unborn") even when missing.
	headTarget := "refs/heads/" + body.DefaultBranch
	headOID, headExists := body.Refs[headTarget]
	if (headExists || wantUnborn) && prefixOK("HEAD", prefixes) {
		var line string
		switch {
		case headExists:
			line = headOID + " HEAD"
		default:
			line = "unborn HEAD"
		}
		if wantSymrefs && body.DefaultBranch != "" {
			line += " symref-target:" + headTarget
		}
		if err := pw.WriteString(line + "\n"); err != nil {
			return err
		}
	}

	// Other refs.
	names := make([]string, 0, len(body.Refs))
	for name := range body.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if name == headTarget && wantSymrefs && headExists {
			// Already represented by HEAD with symref-target annotation;
			// emit the underlying ref line too (real git does).
		}
		if !prefixOK(name, prefixes) {
			continue
		}
		oid := body.Refs[name]
		if err := pw.WriteString(oid + " " + name + "\n"); err != nil {
			return err
		}
	}
	return pw.WriteFlush()
}

func prefixOK(name string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// iterateArgs walks a pkt-line token stream of the shape
//   "command=<cmd>\n"
//   delim
//   "<arg-line>\n" ...
//   flush
// invoking fn for each <arg-line> with the trailing newline stripped.
func iterateArgs(args []pktline.Token, expectCmd string, fn func(line string) error) error {
	if len(args) == 0 {
		return fmt.Errorf("v2proto: empty arg stream")
	}
	if args[0].Type != pktline.Data || strings.TrimRight(string(args[0].Payload), "\n") != "command="+expectCmd {
		return fmt.Errorf("v2proto: expected command=%s, got %q", expectCmd, args[0].Payload)
	}
	i := 1
	if i < len(args) && args[i].Type == pktline.Delim {
		i++
	}
	for ; i < len(args); i++ {
		t := args[i]
		switch t.Type {
		case pktline.Flush:
			return nil
		case pktline.Data:
			line := strings.TrimRight(string(t.Payload), "\n")
			if err := fn(line); err != nil {
				return err
			}
		default:
			return fmt.Errorf("v2proto: unexpected token %v", t.Type)
		}
	}
	return nil // tolerate missing trailing flush
}
```

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./internal/v2proto/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/v2proto/lsrefs.go internal/v2proto/lsrefs_test.go
git commit -m "M3 v2proto: ls-refs command with symrefs/unborn/ref-prefix support"
```

---

## Task 5: v2proto fetch command — request parsing

**Files:**
- Create: `internal/v2proto/fetch.go`
- Create: `internal/v2proto/fetch_test.go`

`fetch` is the protocol-v2 command that drives clone and fetch. Server reads the request, extracts wants/haves/done/shallow/etc., then asks an injected pack producer to stream a packfile to the client. M3 splits this into two halves: this task implements the request parser and response serializer (band wrappers + ack section), and the next gitcli task supplies the actual `git pack-objects` driver. The handler itself is wired together in Task 15. [protocol-v2 §fetch].

The supported argument set in M3:

| arg | semantics |
|---|---|
| `want <oid>` | client wants this OID (commit/tag) |
| `want-ref <ref>` | server-side ref-name resolution (advertised via `ls-refs=unborn` capability — accepted but resolved to OID before handing to pack-objects) |
| `have <oid>` | client claims to have this OID |
| `done` | terminate negotiation |
| `thin-pack` | client accepts thin packs |
| `no-progress` | suppress band-2 |
| `include-tag` | include reachable tags |
| `ofs-delta` | client supports OFS_DELTA (always true for modern git) |
| `deepen <n>` | shallow clone depth N |
| `deepen-since <time>` | parsed and forwarded (best-effort) |
| `deepen-not <ref>` | parsed and forwarded (best-effort) |
| `shallow <oid>` | record client-side shallow boundary |
| `filter <spec>` | NOT advertised; if seen, rejected with `ERR filter not supported` |
| anything else | rejected as unknown |

- [ ] **Step 1: Write the failing test**

Create `internal/v2proto/fetch_test.go`:

```go
package v2proto

import (
	"reflect"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/pktline"
)

func TestParseFetchArgs_HappyPath(t *testing.T) {
	args := tokensFromLines(
		"command=fetch\n",
		"DELIM",
		"thin-pack\n",
		"no-progress\n",
		"include-tag\n",
		"ofs-delta\n",
		"want 1111111111111111111111111111111111111111\n",
		"want 2222222222222222222222222222222222222222\n",
		"have 3333333333333333333333333333333333333333\n",
		"done\n",
		"FLUSH",
	)
	got, err := ParseFetchArgs(args)
	if err != nil {
		t.Fatalf("ParseFetchArgs: %v", err)
	}
	want := FetchRequest{
		Wants: []string{
			"1111111111111111111111111111111111111111",
			"2222222222222222222222222222222222222222",
		},
		Haves:       []string{"3333333333333333333333333333333333333333"},
		Done:        true,
		ThinPack:    true,
		NoProgress:  true,
		IncludeTag:  true,
		OfsDelta:    true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseFetchArgs:\n got %+v\nwant %+v", got, want)
	}
}

func TestParseFetchArgs_Shallow(t *testing.T) {
	args := tokensFromLines(
		"command=fetch\n",
		"DELIM",
		"want 1111111111111111111111111111111111111111\n",
		"deepen 3\n",
		"shallow 4444444444444444444444444444444444444444\n",
		"FLUSH",
	)
	got, err := ParseFetchArgs(args)
	if err != nil {
		t.Fatalf("ParseFetchArgs: %v", err)
	}
	if got.Depth != 3 {
		t.Fatalf("Depth: got %d, want 3", got.Depth)
	}
	if !reflect.DeepEqual(got.Shallow, []string{"4444444444444444444444444444444444444444"}) {
		t.Fatalf("Shallow: got %v", got.Shallow)
	}
}

func TestParseFetchArgs_RejectsUnknown(t *testing.T) {
	args := tokensFromLines(
		"command=fetch\n",
		"DELIM",
		"want 1111111111111111111111111111111111111111\n",
		"weird-arg\n",
		"FLUSH",
	)
	if _, err := ParseFetchArgs(args); err == nil {
		t.Fatalf("ParseFetchArgs: expected error on unknown arg")
	}
}

func TestParseFetchArgs_RejectsFilter(t *testing.T) {
	args := tokensFromLines(
		"command=fetch\n",
		"DELIM",
		"want 1111111111111111111111111111111111111111\n",
		"filter blob:none\n",
		"FLUSH",
	)
	if _, err := ParseFetchArgs(args); err == nil {
		t.Fatalf("ParseFetchArgs: expected error on filter (not advertised)")
	}
}

func TestParseFetchArgs_RejectsBadOID(t *testing.T) {
	args := tokensFromLines(
		"command=fetch\n",
		"DELIM",
		"want notahex\n",
		"FLUSH",
	)
	if _, err := ParseFetchArgs(args); err == nil {
		t.Fatalf("ParseFetchArgs: expected error on bad OID")
	}
}

func TestParseFetchArgs_RequiresWant(t *testing.T) {
	args := tokensFromLines(
		"command=fetch\n",
		"DELIM",
		"done\n",
		"FLUSH",
	)
	if _, err := ParseFetchArgs(args); err == nil {
		t.Fatalf("ParseFetchArgs: expected error when no want present")
	}
}

func TestWriteAcknowledgments_AllUnknown(t *testing.T) {
	var sb strings.Builder
	if err := WriteAcknowledgments(&sb, nil, []string{"3333333333333333333333333333333333333333"}); err != nil {
		t.Fatalf("WriteAcknowledgments: %v", err)
	}
	out := drainPayloads(t, asReader(sb.String()))
	want := []string{"acknowledgments\n", "NAK\n"}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("ack stream: got %v, want %v", out, want)
	}
}

func TestWriteAcknowledgments_SomeCommon(t *testing.T) {
	var sb strings.Builder
	commons := []string{"3333333333333333333333333333333333333333"}
	if err := WriteAcknowledgments(&sb, commons, nil); err != nil {
		t.Fatalf("WriteAcknowledgments: %v", err)
	}
	out := drainPayloads(t, asReader(sb.String()))
	want := []string{
		"acknowledgments\n",
		"ACK 3333333333333333333333333333333333333333 common\n",
		"ready\n",
	}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("ack stream: got %v, want %v", out, want)
	}
}

func asReader(s string) *strings.Reader { return strings.NewReader(s) }

// drainPayloads on *strings.Reader for ack tests
func init() {
	_ = pktline.Data
}
```

The drainPayloads helper used in `lsrefs_test.go` takes a `*bytes.Buffer`. We need a pktline-reader-from-string variant for the ack tests; add it to the existing lsrefs_test.go's helpers OR write a thin local wrapper inside fetch_test.go. Choose the local wrapper to avoid cross-file edits:

Append to `internal/v2proto/fetch_test.go`:

```go
func drainPayloadsFromStrings(t *testing.T, r *strings.Reader) []string {
	t.Helper()
	pr := pktline.NewReader(r)
	var out []string
	for {
		tok, err := pr.Read()
		if err != nil {
			break
		}
		if tok.Type == pktline.Data {
			out = append(out, string(tok.Payload))
		}
	}
	return out
}
```

And update the two ack tests to use `drainPayloadsFromStrings(t, asReader(sb.String()))` instead of `drainPayloads(t, asReader(...))`. Final test file shape:

```go
// (replace the two ack tests' drainPayloads calls accordingly)
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/v2proto/...`
Expected: FAIL with `undefined: ParseFetchArgs`, `undefined: WriteAcknowledgments`, `undefined: FetchRequest`.

- [ ] **Step 3: Write the implementation**

Create `internal/v2proto/fetch.go`:

```go
package v2proto

import (
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/pktline"
)

// FetchRequest is the parsed form of a protocol-v2 "fetch" command body.
type FetchRequest struct {
	Wants       []string // raw 40-char SHA-1 hex
	WantRefs    []string // ref names (server-side resolution)
	Haves       []string // raw 40-char SHA-1 hex
	Done        bool
	ThinPack    bool
	NoProgress  bool
	IncludeTag  bool
	OfsDelta    bool
	Depth       int      // 0 = no shallow
	DeepenSince string
	DeepenNot   []string
	Shallow     []string // client-side shallow boundary OIDs
}

// ParseFetchArgs decodes the pkt-line stream of a "fetch" command body.
//
// Layout:
//   "command=fetch\n"
//   delim
//   "<arg>\n" repeated
//   flush
func ParseFetchArgs(args []pktline.Token) (FetchRequest, error) {
	var req FetchRequest
	if err := iterateArgs(args, "fetch", func(line string) error {
		switch {
		case line == "thin-pack":
			req.ThinPack = true
		case line == "no-progress":
			req.NoProgress = true
		case line == "include-tag":
			req.IncludeTag = true
		case line == "ofs-delta":
			req.OfsDelta = true
		case line == "done":
			req.Done = true
		case strings.HasPrefix(line, "want "):
			oid := strings.TrimPrefix(line, "want ")
			if !validOID(oid) {
				return fmt.Errorf("fetch: invalid want OID %q", oid)
			}
			req.Wants = append(req.Wants, oid)
		case strings.HasPrefix(line, "want-ref "):
			ref := strings.TrimPrefix(line, "want-ref ")
			if ref == "" || strings.ContainsAny(ref, " \t") {
				return fmt.Errorf("fetch: invalid want-ref %q", ref)
			}
			req.WantRefs = append(req.WantRefs, ref)
		case strings.HasPrefix(line, "have "):
			oid := strings.TrimPrefix(line, "have ")
			if !validOID(oid) {
				return fmt.Errorf("fetch: invalid have OID %q", oid)
			}
			req.Haves = append(req.Haves, oid)
		case strings.HasPrefix(line, "deepen "):
			n, err := strconv.Atoi(strings.TrimPrefix(line, "deepen "))
			if err != nil || n <= 0 {
				return fmt.Errorf("fetch: invalid deepen %q", line)
			}
			req.Depth = n
		case strings.HasPrefix(line, "deepen-since "):
			req.DeepenSince = strings.TrimPrefix(line, "deepen-since ")
		case strings.HasPrefix(line, "deepen-not "):
			req.DeepenNot = append(req.DeepenNot, strings.TrimPrefix(line, "deepen-not "))
		case strings.HasPrefix(line, "shallow "):
			oid := strings.TrimPrefix(line, "shallow ")
			if !validOID(oid) {
				return fmt.Errorf("fetch: invalid shallow OID %q", oid)
			}
			req.Shallow = append(req.Shallow, oid)
		case strings.HasPrefix(line, "filter "):
			return fmt.Errorf("fetch: filter not supported in M3")
		default:
			return fmt.Errorf("fetch: unknown argument %q", line)
		}
		return nil
	}); err != nil {
		return FetchRequest{}, err
	}
	if len(req.Wants) == 0 && len(req.WantRefs) == 0 {
		return FetchRequest{}, fmt.Errorf("fetch: no want present")
	}
	return req, nil
}

func validOID(s string) bool {
	if len(s) != 40 {
		return false
	}
	if _, err := hex.DecodeString(s); err != nil {
		return false
	}
	return true
}

// WriteAcknowledgments writes the protocol-v2 "acknowledgments" section that
// precedes the packfile. commons is the (possibly empty) set of haves the
// server is acknowledging as common ancestors. unknown is the set of haves
// the server does not have.
//
// If len(commons)==0 we emit "NAK"; otherwise we emit ACKs for each common
// plus "ready". A trailing flush is the caller's responsibility.
func WriteAcknowledgments(w io.Writer, commons, unknown []string) error {
	pw := pktline.NewWriter(w)
	if err := pw.WriteString("acknowledgments\n"); err != nil {
		return err
	}
	if len(commons) == 0 {
		return pw.WriteString("NAK\n")
	}
	for _, oid := range commons {
		if err := pw.WriteString("ACK " + oid + " common\n"); err != nil {
			return err
		}
	}
	return pw.WriteString("ready\n")
}
```

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./internal/v2proto/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/v2proto/fetch.go internal/v2proto/fetch_test.go
git commit -m "M3 v2proto: fetch arg parser + acknowledgments serializer"
```

---

## Task 6: gitcli additions for the fetch path

**Files:**
- Modify: `internal/gitcli/gitcli.go`
- Modify: `internal/gitcli/gitcli_test.go`

The fetch handler shells out to `git pack-objects --revs --thin --stdout` against the mirror. Inputs (wants and `^haves`) go on stdin. Output is a packfile streamed to the caller. The wrapper hides the pipe plumbing and signals concurrency-safe.

We also add `RevParseObjectKind(ctx, dir, oid)` to validate that a `want` resolves to a commit/tag/tree/blob (M3 only ships commit/tag wants from clients, but the type check is generic).

- [ ] **Step 1: Write the failing test**

Append to `internal/gitcli/gitcli_test.go`:

```go
func TestPackObjectsForFetch_RoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	dir := t.TempDir()
	bare := filepath.Join(dir, "src.git")

	mustGit(t, dir, "init", "--bare", bare)
	work := filepath.Join(dir, "wt")
	mustGit(t, dir, "clone", bare, work)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustGit(t, work, "add", ".")
	mustGit(t, work, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "init")
	mustGit(t, work, "push", "origin", "HEAD:refs/heads/main")
	tip := strings.TrimSpace(string(mustGitCapture(t, work, "rev-parse", "HEAD")))

	ctx := context.Background()
	out, err := PackObjectsForFetch(ctx, bare, PackForFetchOptions{
		Wants:    []string{tip},
		Haves:    nil,
		ThinPack: true,
		IncludeTag: true,
		OfsDelta: true,
	})
	if err != nil {
		t.Fatalf("PackObjectsForFetch: %v", err)
	}
	defer out.Close()

	// Pipe into a fresh bare repo via index-pack to verify validity.
	dst := filepath.Join(dir, "dst.git")
	if err := InitBare(ctx, dst); err != nil {
		t.Fatalf("InitBare: %v", err)
	}
	packDir := filepath.Join(dst, "objects", "pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	packPath := filepath.Join(packDir, "pack-test.pack")
	f, err := os.Create(packPath)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := io.Copy(f, out); err != nil {
		t.Fatalf("copy: %v", err)
	}
	f.Close()
	if err := IndexPack(ctx, dst, packPath); err != nil {
		t.Fatalf("IndexPack: %v", err)
	}
}

func TestRevParseObjectKind_CommitTagBlob(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	dir := t.TempDir()
	bare := filepath.Join(dir, "src.git")
	mustGit(t, dir, "init", "--bare", bare)
	work := filepath.Join(dir, "wt")
	mustGit(t, dir, "clone", bare, work)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustGit(t, work, "add", ".")
	mustGit(t, work, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "x")
	mustGit(t, work, "tag", "v1")
	mustGit(t, work, "push", "origin", "HEAD:refs/heads/main", "tag", "v1")

	ctx := context.Background()
	commitOID := strings.TrimSpace(string(mustGitCapture(t, work, "rev-parse", "HEAD")))
	tagOID := strings.TrimSpace(string(mustGitCapture(t, work, "rev-parse", "v1")))

	if k, err := RevParseObjectKind(ctx, bare, commitOID); err != nil || k != "commit" {
		t.Fatalf("commit: kind=%q err=%v", k, err)
	}
	if k, err := RevParseObjectKind(ctx, bare, tagOID); err != nil {
		t.Fatalf("tag: err=%v", err)
	} else if k != "commit" && k != "tag" {
		// lightweight tags resolve to commit; v1 here is lightweight, so commit
		t.Fatalf("tag kind=%q", k)
	}
}

// helper: run git and capture stdout
func mustGitCapture(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	out, err := RunForTest(dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return out
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/gitcli/...`
Expected: FAIL with `undefined: PackObjectsForFetch`, etc.

- [ ] **Step 3: Write the implementation**

Append to `internal/gitcli/gitcli.go`:

```go
// PackForFetchOptions configures PackObjectsForFetch.
type PackForFetchOptions struct {
	Wants       []string
	Haves       []string // ^<oid> exclusion list
	ThinPack    bool
	IncludeTag  bool
	OfsDelta    bool
	NoProgress  bool   // suppress stderr-as-progress
	ShallowFile string // optional path to a temp shallow file for shallow fetches
	Depth       int    // 0 = unbounded
}

// PackObjectsForFetch invokes "git pack-objects --revs --stdout" against the
// bare repo at dir, feeding wants and ^haves via stdin and returning an
// io.ReadCloser over the resulting pack stream. The caller MUST Close() the
// returned reader (which waits for git to exit and surfaces nonzero exit
// status as an error).
//
// Any output on stderr is captured into the returned error on close-failure;
// it is NOT streamed to a side-band by this layer (the caller wraps it).
func PackObjectsForFetch(ctx context.Context, dir string, opts PackForFetchOptions) (io.ReadCloser, error) {
	bin, err := resolveBinary()
	if err != nil {
		return nil, err
	}
	args := []string{"pack-objects", "--revs", "--stdout", "--no-replace-objects"}
	if opts.ThinPack {
		args = append(args, "--thin")
	}
	if opts.IncludeTag {
		args = append(args, "--include-tag")
	}
	if opts.OfsDelta {
		args = append(args, "--delta-base-offset")
	}
	if opts.NoProgress {
		args = append(args, "-q")
	}
	if opts.ShallowFile != "" {
		// pack-objects respects the shallow file via the "shallow" config in
		// the repo dir. We pass it via -c override:
		args = append([]string{"-c", "core.shallow=" + opts.ShallowFile}, args...)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = scrubGitRepoEnv(os.Environ())

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("pack-objects: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("pack-objects: stdout pipe: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("pack-objects: start: %w", err)
	}

	// Write wants and haves to stdin, then close stdin so pack-objects
	// proceeds. We do this synchronously before returning so any input error
	// surfaces immediately rather than racing the reader.
	go func() {
		defer stdin.Close()
		bw := bufio.NewWriter(stdin)
		for _, w := range opts.Wants {
			fmt.Fprintf(bw, "%s\n", w)
		}
		for _, h := range opts.Haves {
			fmt.Fprintf(bw, "^%s\n", h)
		}
		_ = bw.Flush()
	}()

	return &packObjectsReader{r: stdout, cmd: cmd, stderr: &stderr}, nil
}

type packObjectsReader struct {
	r      io.ReadCloser
	cmd    *exec.Cmd
	stderr *bytes.Buffer
}

func (p *packObjectsReader) Read(b []byte) (int, error) { return p.r.Read(b) }

func (p *packObjectsReader) Close() error {
	closeErr := p.r.Close()
	waitErr := p.cmd.Wait()
	if waitErr != nil {
		return &runError{
			binary: p.cmd.Path,
			args:   p.cmd.Args[1:],
			cause:  waitErr,
			stderr: redactCreds(p.stderr.String()),
		}
	}
	return closeErr
}

// RevParseObjectKind returns the object type ("commit", "tag", "tree",
// "blob") for the OID in the bare repo at dir, or an error if the object is
// missing or unparseable.
func RevParseObjectKind(ctx context.Context, dir, oid string) (string, error) {
	if !validRefOrOID(oid) {
		return "", fmt.Errorf("rev-parse: invalid oid %q", oid)
	}
	out, err := run(ctx, dir, "cat-file", "-t", oid)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
```

Imports added at the top of `gitcli.go` if not already present: `"bufio"`, `"bytes"`, `"io"`, `"os"`, `"os/exec"`. Most are already imported.

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./internal/gitcli/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gitcli/gitcli.go internal/gitcli/gitcli_test.go
git commit -m "M3 gitcli: PackObjectsForFetch (stream pack to client) + RevParseObjectKind"
```

---

## Task 7: mirror — lazy materialization and stale detection

**Files:**
- Create: `internal/mirror/mirror.go`
- Create: `internal/mirror/mirror_test.go`

A `*mirror.Mirror` represents the per-repo on-disk bare-repo cache. `Open` locates `<root>/<tenant>/<repo>/`, reads `manifest_version.txt`, compares to bucket's current root manifest version, and either uses the existing `bare/` or rebuilds it via the M2 exporter. The mutex + flock + manager glue land in Task 8.

The bucket's current manifest version comes from `repo.Repo.ReadRoot(ctx)` which returns a `*RootView` that exposes the version (an opaque string from the storage layer's `WithVersion`). We persist that string verbatim in `manifest_version.txt`.

- [ ] **Step 1: Write the failing test**

Create `internal/mirror/mirror_test.go`:

```go
package mirror

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/exporter"
	"github.com/bucketvcs/bucketvcs/internal/importer"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// makeImportedRepo imports a tiny synthetic repo into a localfs store and
// returns (store, tenant, repoID).
func makeImportedRepo(t *testing.T) (string, string, string) {
	t.Helper()
	storeDir := t.TempDir()
	srcWork := t.TempDir()
	srcBare := filepath.Join(t.TempDir(), "src.git")

	mustCmd(t, "git", "init", "--bare", srcBare)
	mustCmd(t, "git", "clone", srcBare, srcWork)
	if err := os.WriteFile(filepath.Join(srcWork, "a.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustCmdIn(t, srcWork, "git", "add", ".")
	mustCmdIn(t, srcWork, "git", "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "init")
	mustCmdIn(t, srcWork, "git", "push", "origin", "HEAD:refs/heads/main")

	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := importer.Import(context.Background(), store, "acme", "demo", srcBare); err != nil {
		t.Fatalf("Import: %v", err)
	}
	return storeDir, "acme", "demo"
}

func TestMirror_LazyMaterializeFromExporter(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	storeDir, tenant, repoID := makeImportedRepo(t)
	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	root := t.TempDir()
	m, err := openForTest(context.Background(), root, store, tenant, repoID)
	if err != nil {
		t.Fatalf("openForTest: %v", err)
	}
	bare := m.BareDir()
	if _, err := os.Stat(filepath.Join(bare, "HEAD")); err != nil {
		t.Fatalf("bare repo not materialized: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, tenant, repoID, "manifest_version.txt")); err != nil {
		t.Fatalf("manifest_version.txt not written: %v", err)
	}

	// Second open: should be cached, no rebuild.
	m2, err := openForTest(context.Background(), root, store, tenant, repoID)
	if err != nil {
		t.Fatalf("openForTest second: %v", err)
	}
	if m2.BareDir() != bare {
		t.Fatalf("BareDir changed across opens: %q vs %q", m2.BareDir(), bare)
	}
}

func TestMirror_StaleDetectionRebuilds(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	storeDir, tenant, repoID := makeImportedRepo(t)
	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	root := t.TempDir()
	if _, err := openForTest(context.Background(), root, store, tenant, repoID); err != nil {
		t.Fatalf("first open: %v", err)
	}
	versPath := filepath.Join(root, tenant, repoID, "manifest_version.txt")
	if err := os.WriteFile(versPath, []byte("DIFFERENT-VERSION-FAKE"), 0o644); err != nil {
		t.Fatalf("corrupt sentinel: %v", err)
	}
	// Second open should detect mismatch and rebuild (no error).
	if _, err := openForTest(context.Background(), root, store, tenant, repoID); err != nil {
		t.Fatalf("second open after sentinel mismatch: %v", err)
	}
	got, err := os.ReadFile(versPath)
	if err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	if string(got) == "DIFFERENT-VERSION-FAKE" {
		t.Fatalf("sentinel not rewritten after rebuild")
	}
}

func TestMirror_RejectsBadTenantOrRepo(t *testing.T) {
	root := t.TempDir()
	store, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, bad := range []string{"", "../etc", "with space", "a/b"} {
		if _, err := openForTest(context.Background(), root, store, bad, "ok"); err == nil {
			t.Fatalf("openForTest tenant=%q: expected error", bad)
		}
		if _, err := openForTest(context.Background(), root, store, "ok", bad); err == nil {
			t.Fatalf("openForTest repo=%q: expected error", bad)
		}
	}
	// silence unused-import warnings if any
	_ = exporter.Export
	_ = strings.HasPrefix
}

func mustCmd(t *testing.T, args ...string)               { execHelper(t, "", args...) }
func mustCmdIn(t *testing.T, dir string, args ...string) { execHelper(t, dir, args...) }
func execHelper(t *testing.T, dir string, args ...string) {
	t.Helper()
	out, err := runCmd(dir, args[0], args[1:]...)
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
}
```

Add a small test-only helper file `internal/mirror/runcmd_test.go`:

```go
package mirror

import (
	"bytes"
	"os/exec"
)

func runCmd(dir, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.Bytes(), err
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/mirror/...`
Expected: FAIL with `undefined: openForTest`, `undefined: BareDir`, etc.

- [ ] **Step 3: Write the implementation**

Create `internal/mirror/mirror.go`:

```go
// Package mirror manages per-repo on-disk bare-repo caches that the gateway
// uses for `git pack-objects` (fetch) and `git index-pack` (push). The
// authoritative state lives in the bucket; the mirror is a derived view that
// can be wiped and rebuilt from the M2 exporter at any time.
package mirror

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"github.com/bucketvcs/bucketvcs/internal/exporter"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// nameRE matches valid tenant/repo identifiers per spec §10 (Section 10
// "URL routing" of the M3 design doc).
var nameRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Mirror is the per-repo on-disk bare-repo cache.
type Mirror struct {
	root    string // <root>/<tenant>/<repo>/
	tenant  string
	repoID  string
	store   storage.ObjectStore

	mu sync.RWMutex
}

// BareDir returns the absolute path to the bare git repo (suitable for
// `git -C <BareDir>` invocations).
func (m *Mirror) BareDir() string { return filepath.Join(m.root, "bare") }

// VersionFile returns the absolute path to the manifest-version sentinel.
func (m *Mirror) VersionFile() string { return filepath.Join(m.root, "manifest_version.txt") }

// IncomingDir returns the per-repo staging dir for inbound packs (push).
func (m *Mirror) IncomingDir() string { return filepath.Join(m.root, "incoming") }

// openForTest is the in-package entry point used by tests. The Manager in
// Task 8 wraps this with a per-repo mutex map and a process-wide flock.
func openForTest(ctx context.Context, rootDir string, store storage.ObjectStore, tenant, repoID string) (*Mirror, error) {
	if !nameRE.MatchString(tenant) {
		return nil, fmt.Errorf("mirror: invalid tenant %q", tenant)
	}
	if !nameRE.MatchString(repoID) {
		return nil, fmt.Errorf("mirror: invalid repoID %q", repoID)
	}
	root := filepath.Join(rootDir, tenant, repoID)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("mirror: mkdir root: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "incoming"), 0o755); err != nil {
		return nil, fmt.Errorf("mirror: mkdir incoming: %w", err)
	}
	m := &Mirror{root: root, tenant: tenant, repoID: repoID, store: store}
	if err := m.SyncToCurrent(ctx); err != nil {
		return nil, err
	}
	return m, nil
}

// SyncToCurrent compares the on-disk sentinel against the bucket's current
// root manifest version. If they match and bare/ exists, returns nil. If
// they don't, wipes and rebuilds bare/ via the M2 exporter, and writes the
// new sentinel.
func (m *Mirror) SyncToCurrent(ctx context.Context) error {
	r, err := repo.Open(ctx, m.store, m.tenant, m.repoID)
	if err != nil {
		return fmt.Errorf("mirror: repo.Open: %w", err)
	}
	view, err := r.ReadRoot(ctx)
	if err != nil {
		return fmt.Errorf("mirror: repo.ReadRoot: %w", err)
	}
	want := view.Version

	bareExists := dirExists(m.BareDir())
	gotSentinel, _ := os.ReadFile(m.VersionFile())
	if bareExists && string(gotSentinel) == want {
		return nil
	}

	// Stale or absent: wipe and rebuild.
	if bareExists {
		if err := os.RemoveAll(m.BareDir()); err != nil {
			return fmt.Errorf("mirror: wipe bare: %w", err)
		}
	}
	// Remove a stale sentinel so a partial rebuild doesn't leave it pointing
	// at something inconsistent.
	_ = os.Remove(m.VersionFile())
	if err := exporter.Export(ctx, m.store, m.tenant, m.repoID, m.BareDir(), exporter.Options{}); err != nil {
		return fmt.Errorf("mirror: exporter.Export: %w", err)
	}
	if err := atomicWrite(m.VersionFile(), []byte(want)); err != nil {
		return fmt.Errorf("mirror: write sentinel: %w", err)
	}
	return nil
}

// CurrentVersion reads the on-disk sentinel. Used by callers that want to
// double-check post-update.
func (m *Mirror) CurrentVersion() (string, error) {
	b, err := os.ReadFile(m.VersionFile())
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Suppress unused-import warning when the build excludes errors:
var _ = errors.New
```

The `exporter.Export` signature must match what M2 ships. If M2's exporter exposes only the `Export(ctx, store, tenant, repoID, dest)` shape, drop the `exporter.Options{}` arg. Verify with `grep -n "^func Export" internal/exporter/exporter.go` before committing.

Also `repo.RootView.Version` must be the exposed field name; if M1 uses a different name (e.g. `ManifestVersion`), update accordingly.

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./internal/mirror/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/mirror/mirror.go internal/mirror/mirror_test.go internal/mirror/runcmd_test.go
git commit -m "M3 mirror: lazy materialization + stale detection via M2 exporter"
```

---

## Task 8: mirror — Manager with per-repo mutex map and process flock

**Files:**
- Create: `internal/mirror/lock.go`
- Modify: `internal/mirror/mirror.go` (add `Manager` type, change `openForTest` to call into manager)
- Create: `internal/mirror/lock_test.go`

The HTTP handler chain wants one entry point: `manager.Open(ctx, tenant, repo)` → returns a `*Mirror` whose `RLock`/`RUnlock`/`Lock`/`Unlock` are per-repo. The `Manager` owns:
- A `sync.Mutex` over a `map[string]*Mirror` — first-access creates the entry.
- A process-wide `flock` on `<root>/.bucketvcs-mirror-lock` to refuse double-start. Using `golang.org/x/sys/unix.Flock` keeps it cross-distro on Linux.

- [ ] **Step 1: Write the failing test**

Create `internal/mirror/lock_test.go`:

```go
package mirror

import (
	"context"
	"os/exec"
	"runtime"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestManager_DoubleStartRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("flock test is unix-only")
	}
	root := t.TempDir()
	store, _ := localfs.Open(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })

	mgr1, err := NewManager(root, store)
	if err != nil {
		t.Fatalf("NewManager 1: %v", err)
	}
	t.Cleanup(func() { _ = mgr1.Close() })
	if _, err := NewManager(root, store); err == nil {
		t.Fatalf("NewManager 2: expected error on double start")
	}
}

func TestManager_SameMirrorAcrossOpens(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	storeDir, tenant, repoID := makeImportedRepo(t)
	store, _ := localfs.Open(storeDir)
	t.Cleanup(func() { _ = store.Close() })

	root := t.TempDir()
	mgr, err := NewManager(root, store)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	m1, err := mgr.Open(context.Background(), tenant, repoID)
	if err != nil {
		t.Fatalf("Open 1: %v", err)
	}
	m2, err := mgr.Open(context.Background(), tenant, repoID)
	if err != nil {
		t.Fatalf("Open 2: %v", err)
	}
	if m1 != m2 {
		t.Fatalf("Open returned different *Mirror values for same (tenant, repo)")
	}
}

// keep import alive
var _ = exec.Command
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/mirror/...`
Expected: FAIL with `undefined: NewManager`, `undefined: Manager.Open`.

- [ ] **Step 3: Write the implementation**

Create `internal/mirror/lock.go`:

```go
//go:build unix

package mirror

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// acquireLock returns an open *os.File holding an exclusive non-blocking
// flock on path. If the lock is already held by another process, it returns
// an error.
func acquireLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("mirror: open lock: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("mirror: another bucketvcs serve is using %s: %w", path, err)
	}
	return f, nil
}

func releaseLock(f *os.File) error {
	if f == nil {
		return nil
	}
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return f.Close()
}
```

If `golang.org/x/sys/unix` is not in go.mod, run `go get golang.org/x/sys/unix` before this task.

Append to `internal/mirror/mirror.go`:

```go
// Manager owns the per-process collection of mirrors. Construct one at
// gateway startup; close on shutdown.
type Manager struct {
	rootDir string
	store   storage.ObjectStore
	lock    *os.File

	mu      sync.Mutex
	mirrors map[string]*Mirror
}

// NewManager creates the manager rooted at rootDir. It acquires a
// process-wide flock on <rootDir>/.bucketvcs-mirror-lock to refuse double-
// start.
func NewManager(rootDir string, store storage.ObjectStore) (*Manager, error) {
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return nil, fmt.Errorf("mirror: mkdir root: %w", err)
	}
	lf, err := acquireLock(filepath.Join(rootDir, ".bucketvcs-mirror-lock"))
	if err != nil {
		return nil, err
	}
	return &Manager{
		rootDir: rootDir,
		store:   store,
		lock:    lf,
		mirrors: map[string]*Mirror{},
	}, nil
}

// Open returns the *Mirror for (tenant, repoID), creating and lazy-
// materializing it on first use. Subsequent calls return the same value.
// The returned Mirror's mutex is unique per (tenant, repoID).
func (mg *Manager) Open(ctx context.Context, tenant, repoID string) (*Mirror, error) {
	if !nameRE.MatchString(tenant) {
		return nil, fmt.Errorf("mirror: invalid tenant %q", tenant)
	}
	if !nameRE.MatchString(repoID) {
		return nil, fmt.Errorf("mirror: invalid repoID %q", repoID)
	}
	key := tenant + "/" + repoID

	mg.mu.Lock()
	if m, ok := mg.mirrors[key]; ok {
		mg.mu.Unlock()
		// Always sync on hot path: cheap when current.
		if err := m.SyncToCurrent(ctx); err != nil {
			return nil, err
		}
		return m, nil
	}
	mg.mu.Unlock()

	// Create outside the manager lock so two different repos don't block
	// each other on the slow exporter.
	root := filepath.Join(mg.rootDir, tenant, repoID)
	if err := os.MkdirAll(filepath.Join(root, "incoming"), 0o755); err != nil {
		return nil, fmt.Errorf("mirror: mkdir incoming: %w", err)
	}
	m := &Mirror{root: root, tenant: tenant, repoID: repoID, store: mg.store}
	if err := m.SyncToCurrent(ctx); err != nil {
		return nil, err
	}
	mg.mu.Lock()
	if existing, ok := mg.mirrors[key]; ok {
		// Race: another caller created in parallel. Use theirs.
		mg.mu.Unlock()
		return existing, nil
	}
	mg.mirrors[key] = m
	mg.mu.Unlock()
	return m, nil
}

// Close releases the process flock. It does not delete on-disk mirrors.
func (mg *Manager) Close() error { return releaseLock(mg.lock) }

// RLock / RUnlock / Lock / Unlock expose the per-repo mutex.
func (m *Mirror) RLock()   { m.mu.RLock() }
func (m *Mirror) RUnlock() { m.mu.RUnlock() }
func (m *Mirror) Lock()    { m.mu.Lock() }
func (m *Mirror) Unlock()  { m.mu.Unlock() }
```

Update `openForTest` in mirror.go to delegate through a Manager (so test paths exercise the same code):

```go
func openForTest(ctx context.Context, rootDir string, store storage.ObjectStore, tenant, repoID string) (*Mirror, error) {
	mg, err := NewManager(rootDir, store)
	if err != nil {
		return nil, err
	}
	// Manager is intentionally leaked in this test helper; tests rely on
	// process-exit to release the flock. For the lock_test cases we use
	// NewManager directly.
	return mg.Open(ctx, tenant, repoID)
}
```

The tests that already use `openForTest` continue to work; the lock_test cases use `NewManager` directly because they assert on the lock semantics.

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./internal/mirror/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/mirror/mirror.go internal/mirror/lock.go internal/mirror/lock_test.go
git commit -m "M3 mirror: Manager with per-repo mutex map and process flock"
```

---

## Task 9: mirror — IngestPack

**Files:**
- Create: `internal/mirror/ingest.go`
- Create: `internal/mirror/ingest_test.go`

`IngestPack` is the single mutation entry point used by the push handler after pack validation succeeds. It copies the validated pack into `<bare>/objects/pack/`, runs `git update-ref` for each ref command, and writes the new manifest-version sentinel. Caller must hold `m.Lock()` (verified at runtime via a `lockChecker` flag set by `Lock`/`Unlock`).

- [ ] **Step 1: Write the failing test**

Create `internal/mirror/ingest_test.go`:

```go
package mirror

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestIngestPack_CopiesAndUpdatesRefs(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	storeDir, tenant, repoID := makeImportedRepo(t)
	store, _ := localfs.Open(storeDir)
	t.Cleanup(func() { _ = store.Close() })

	root := t.TempDir()
	mgr, err := NewManager(root, store)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	m, err := mgr.Open(context.Background(), tenant, repoID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Build a new pack inside the mirror by adding one more commit.
	bare := m.BareDir()
	work := filepath.Join(t.TempDir(), "wt")
	mustCmd(t, "git", "clone", bare, work)
	if err := os.WriteFile(filepath.Join(work, "b.txt"), []byte("more\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustCmdIn(t, work, "git", "add", ".")
	mustCmdIn(t, work, "git", "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "more")
	newOID := strings.TrimSpace(string(mustCmdCapture(t, work, "git", "rev-parse", "HEAD")))
	oldOID := strings.TrimSpace(string(mustCmdCapture(t, bare, "git", "rev-parse", "refs/heads/main")))

	// Pack the new commit only.
	tmpPack := filepath.Join(t.TempDir(), "new")
	mustCmdIn(t, work, "bash", "-c",
		"git rev-list "+newOID+" ^"+oldOID+" | git pack-objects "+tmpPack)
	// pack-objects emits hash-named files; locate them
	matches, err := filepath.Glob(tmpPack + "-*.pack")
	if err != nil || len(matches) != 1 {
		t.Fatalf("pack-objects produced %v err=%v", matches, err)
	}
	packPath := matches[0]

	m.Lock()
	defer m.Unlock()
	updates := []RefUpdate{{
		Refname: "refs/heads/main",
		OldOID:  oldOID,
		NewOID:  newOID,
	}}
	if err := m.IngestPack(context.Background(), packPath, updates, "fake-new-version"); err != nil {
		t.Fatalf("IngestPack: %v", err)
	}

	got := strings.TrimSpace(string(mustCmdCapture(t, bare, "git", "rev-parse", "refs/heads/main")))
	if got != newOID {
		t.Fatalf("ref not updated: got %s, want %s", got, newOID)
	}
	v, err := m.CurrentVersion()
	if err != nil || v != "fake-new-version" {
		t.Fatalf("sentinel: got %q err=%v", v, err)
	}
}

func TestIngestPack_DeleteRef(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	storeDir, tenant, repoID := makeImportedRepo(t)
	store, _ := localfs.Open(storeDir)
	t.Cleanup(func() { _ = store.Close() })

	root := t.TempDir()
	mgr, err := NewManager(root, store)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	m, _ := mgr.Open(context.Background(), tenant, repoID)

	// Create a second branch to delete.
	bare := m.BareDir()
	mainOID := strings.TrimSpace(string(mustCmdCapture(t, bare, "git", "rev-parse", "refs/heads/main")))
	mustCmdIn(t, bare, "git", "update-ref", "refs/heads/doomed", mainOID)

	m.Lock()
	defer m.Unlock()
	if err := m.IngestPack(context.Background(), "", []RefUpdate{{
		Refname: "refs/heads/doomed",
		OldOID:  mainOID,
		NewOID:  "0000000000000000000000000000000000000000",
	}}, "v2"); err != nil {
		t.Fatalf("IngestPack delete: %v", err)
	}
	out, err := exec.Command("git", "-C", bare, "rev-parse", "--verify", "refs/heads/doomed").Output()
	if err == nil {
		t.Fatalf("ref still present: %s", out)
	}
}

// random pack-name suffix helper if needed
func randHex(n int) string { b := make([]byte, n); _, _ = rand.Read(b); return hex.EncodeToString(b) }

// helpers from earlier tests, repeated for visibility:
import_helpers_already_in_mirror_test_go := true
_ = import_helpers_already_in_mirror_test_go
```

The `mustCmdCapture` helper isn't defined yet; add to the existing `runcmd_test.go`:

```go
func mustCmdCapture(t *testing.T, dir, name string, args ...string) []byte {
	t.Helper()
	out, err := runCmd(dir, name, args...)
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return out
}
```

Also add `import "strings"` and `import "os/exec"` to `ingest_test.go` and remove the redeclared `randHex` if it conflicts.

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/mirror/...`
Expected: FAIL with `undefined: RefUpdate`, `undefined: IngestPack`.

- [ ] **Step 3: Write the implementation**

Create `internal/mirror/ingest.go`:

```go
package mirror

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

// RefUpdate describes one ref change to apply to the mirror.
type RefUpdate struct {
	Refname string
	OldOID  string // 40-char hex; "" or 40 zeros for create
	NewOID  string // 40-char hex; 40 zeros for delete
}

// nullOID is the sentinel for create/delete commands.
const nullOID = "0000000000000000000000000000000000000000"

// IngestPack copies packPath (with companion .idx — caller must have run
// `git index-pack` to produce it) into the mirror's objects/pack/ dir, then
// runs git update-ref for each update, then writes newVersion to the
// manifest_version sentinel. Caller MUST hold m.Lock().
//
// If packPath == "" (delete-only push), the pack copy is skipped.
//
// On any partial-failure (a copy succeeded but a ref update failed), the
// mirror is left in the partial state; the next request will detect the
// stale sentinel (we never bump it on partial failure) and rebuild from the
// bucket. The pack file remains as a dangling object; harmless until GC.
func (m *Mirror) IngestPack(ctx context.Context, packPath string, updates []RefUpdate, newVersion string) error {
	if packPath != "" {
		if err := copyPackPair(packPath, filepath.Join(m.BareDir(), "objects", "pack")); err != nil {
			return fmt.Errorf("mirror: copy pack: %w", err)
		}
	}
	for _, u := range updates {
		if err := applyRefUpdate(ctx, m.BareDir(), u); err != nil {
			return fmt.Errorf("mirror: ref update %q: %w", u.Refname, err)
		}
	}
	if err := atomicWrite(m.VersionFile(), []byte(newVersion)); err != nil {
		return fmt.Errorf("mirror: write sentinel: %w", err)
	}
	return nil
}

func copyPackPair(packPath, destDir string) error {
	if !strings.HasSuffix(packPath, ".pack") {
		return fmt.Errorf("mirror: pack path must end in .pack, got %q", packPath)
	}
	idxPath := strings.TrimSuffix(packPath, ".pack") + ".idx"
	if _, err := os.Stat(idxPath); err != nil {
		return fmt.Errorf("mirror: missing companion .idx for %q: %w", packPath, err)
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	for _, src := range []string{packPath, idxPath} {
		base := filepath.Base(src)
		if err := copyFile(src, filepath.Join(destDir, base)); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		// Allow idempotent retry: if the file already exists with the same
		// content we accept it. Otherwise fail.
		if os.IsExist(err) {
			return nil
		}
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return nil
}

func applyRefUpdate(ctx context.Context, bareDir string, u RefUpdate) error {
	switch {
	case u.NewOID == nullOID:
		return gitcli.UpdateRefDelete(ctx, bareDir, u.Refname, u.OldOID)
	case u.OldOID == "" || u.OldOID == nullOID:
		return gitcli.UpdateRef(ctx, bareDir, u.Refname, u.NewOID)
	default:
		return gitcli.UpdateRefCAS(ctx, bareDir, u.Refname, u.NewOID, u.OldOID)
	}
}
```

This file references `gitcli.UpdateRefDelete` and `gitcli.UpdateRefCAS` which are added in Task 10. Until then the package won't build. That's fine for the staged TDD workflow — the failing-test step in this task can be a build-fail, and Task 10 makes both green.

To make this task self-contained anyway, add minimal stubs to `internal/gitcli/gitcli.go` for now:

```go
func UpdateRefDelete(ctx context.Context, dir, ref, oldOID string) error {
	if !validRefOrOID(ref) || !validRefOrOID(oldOID) {
		return fmt.Errorf("update-ref: invalid args")
	}
	_, err := run(ctx, dir, "update-ref", "-d", ref, oldOID)
	return err
}

func UpdateRefCAS(ctx context.Context, dir, ref, newOID, oldOID string) error {
	if !validRefOrOID(ref) || !validRefOrOID(newOID) || !validRefOrOID(oldOID) {
		return fmt.Errorf("update-ref: invalid args")
	}
	_, err := run(ctx, dir, "update-ref", ref, newOID, oldOID)
	return err
}
```

Task 10 promotes these to first-class with tests.

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./internal/mirror/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/mirror/ingest.go internal/mirror/ingest_test.go internal/mirror/runcmd_test.go internal/gitcli/gitcli.go
git commit -m "M3 mirror: IngestPack copies pack + applies ref updates + bumps sentinel"
```

---

## Task 10: gitcli additions for the push path

**Files:**
- Modify: `internal/gitcli/gitcli.go`
- Modify: `internal/gitcli/gitcli_test.go`

The push handler validates an inbound pack via `git index-pack --strict --fix-thin --keep` against the mirror, then connectivity-checks via `git rev-list --objects --not --all <new-oids>` (zero output ⇒ all parents/trees/blobs are reachable), then applies ref updates. This task adds the wrappers (`IndexPackStrict`, `FsckConnectivityOnly`, `RevListNotAll`) and promotes the stub `UpdateRefDelete`/`UpdateRefCAS` from Task 9 into tested helpers.

- [ ] **Step 1: Write the failing test**

Append to `internal/gitcli/gitcli_test.go`:

```go
func TestIndexPackStrict_RoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	dir := t.TempDir()
	bare := filepath.Join(dir, "src.git")
	mustGit(t, dir, "init", "--bare", bare)
	work := filepath.Join(dir, "wt")
	mustGit(t, dir, "clone", bare, work)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustGit(t, work, "add", ".")
	mustGit(t, work, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "x")
	mustGit(t, work, "push", "origin", "HEAD:refs/heads/main")
	tip := strings.TrimSpace(string(mustGitCapture(t, work, "rev-parse", "HEAD")))

	// Build a thin pack from src/bare into a tempfile.
	ctx := context.Background()
	r, err := PackObjectsForFetch(ctx, bare, PackForFetchOptions{Wants: []string{tip}, ThinPack: true, OfsDelta: true})
	if err != nil {
		t.Fatalf("PackObjectsForFetch: %v", err)
	}
	defer r.Close()
	tmp := filepath.Join(dir, "incoming.pack")
	f, _ := os.Create(tmp)
	if _, err := io.Copy(f, r); err != nil {
		t.Fatalf("copy: %v", err)
	}
	f.Close()

	// Index it into a fresh bare.
	dst := filepath.Join(dir, "dst.git")
	if err := InitBare(ctx, dst); err != nil {
		t.Fatalf("InitBare: %v", err)
	}
	idxPath, err := IndexPackStrict(ctx, dst, tmp)
	if err != nil {
		t.Fatalf("IndexPackStrict: %v", err)
	}
	if _, err := os.Stat(idxPath); err != nil {
		t.Fatalf("idx not present: %v", err)
	}
}

func TestIndexPackStrict_RejectsCorruptPack(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	dir := t.TempDir()
	dst := filepath.Join(dir, "dst.git")
	if err := InitBare(context.Background(), dst); err != nil {
		t.Fatalf("InitBare: %v", err)
	}
	bad := filepath.Join(dir, "bad.pack")
	if err := os.WriteFile(bad, []byte("PACK\x00\x00\x00\x02not a real pack"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := IndexPackStrict(context.Background(), dst, bad); err == nil {
		t.Fatalf("IndexPackStrict: expected error on corrupt pack")
	}
}

func TestRevListNotAll_EmptyMeansClean(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	dir := t.TempDir()
	bare := filepath.Join(dir, "src.git")
	mustGit(t, dir, "init", "--bare", bare)
	work := filepath.Join(dir, "wt")
	mustGit(t, dir, "clone", bare, work)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustGit(t, work, "add", ".")
	mustGit(t, work, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "x")
	mustGit(t, work, "push", "origin", "HEAD:refs/heads/main")
	tip := strings.TrimSpace(string(mustGitCapture(t, work, "rev-parse", "HEAD")))

	missing, err := RevListNotAll(context.Background(), bare, []string{tip})
	if err != nil {
		t.Fatalf("RevListNotAll: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("RevListNotAll: expected empty, got %v", missing)
	}
}

func TestUpdateRefDelete(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	dir := t.TempDir()
	bare := filepath.Join(dir, "src.git")
	mustGit(t, dir, "init", "--bare", bare)
	mustGit(t, bare, "update-ref", "refs/heads/temp", "0000000000000000000000000000000000000000", "")
	// the line above won't actually create the ref because of empty old; populate via push instead
	work := filepath.Join(dir, "wt")
	mustGit(t, dir, "clone", bare, work)
	_ = os.WriteFile(filepath.Join(work, "a.txt"), []byte("x"), 0o644)
	mustGit(t, work, "add", ".")
	mustGit(t, work, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "x")
	mustGit(t, work, "push", "origin", "HEAD:refs/heads/temp")
	oid := strings.TrimSpace(string(mustGitCapture(t, work, "rev-parse", "HEAD")))

	if err := UpdateRefDelete(context.Background(), bare, "refs/heads/temp", oid); err != nil {
		t.Fatalf("UpdateRefDelete: %v", err)
	}
	out, err := RunForTest(bare, "rev-parse", "--verify", "refs/heads/temp")
	if err == nil {
		t.Fatalf("ref not deleted: %s", out)
	}
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/gitcli/...`
Expected: FAIL with `undefined: IndexPackStrict`, etc. (UpdateRefDelete and UpdateRefCAS were stubbed in Task 9; ensure those tests exercise them.)

- [ ] **Step 3: Write the implementation**

Append to `internal/gitcli/gitcli.go`:

```go
// IndexPackStrict runs `git index-pack --strict --fix-thin --keep` against
// the bare repo at dir, indexing packPath. Returns the path to the produced
// .idx file. The .keep file remains alongside the pack to prevent `git gc`
// from racing the caller; callers MUST remove it after Move/IngestPack.
func IndexPackStrict(ctx context.Context, dir, packPath string) (string, error) {
	out, err := run(ctx, dir, "index-pack", "--strict", "--fix-thin", "--keep", packPath)
	if err != nil {
		return "", err
	}
	_ = out // hash printed on stdout but we derive idx path from packPath
	idx := strings.TrimSuffix(packPath, ".pack") + ".idx"
	if _, err := os.Stat(idx); err != nil {
		return "", fmt.Errorf("index-pack: idx not produced: %w", err)
	}
	return idx, nil
}

// RevListNotAll runs `git rev-list --objects <oids...> --not --all` against
// the bare repo at dir. Returns the list of OIDs that are reachable from
// the given oids but NOT from any current ref. After a successful
// IndexPackStrict, the inbound pack's contents should be the only members of
// this set; if any "missing" OIDs from the pack don't appear, connectivity
// is intact.
//
// In the validation flow we typically just want "any output ⇒ unreachable
// objects exist"; the caller intersects with pack contents to detect
// missing parents/trees/blobs.
func RevListNotAll(ctx context.Context, dir string, oids []string) ([]string, error) {
	for _, o := range oids {
		if !validRefOrOID(o) {
			return nil, fmt.Errorf("rev-list: invalid oid %q", o)
		}
	}
	args := append([]string{"rev-list", "--objects"}, oids...)
	args = append(args, "--not", "--all")
	out, err := run(ctx, dir, args...)
	if err != nil {
		return nil, err
	}
	var found []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		// "<oid> [<path>]"
		if i := strings.IndexByte(line, ' '); i >= 0 {
			found = append(found, line[:i])
		} else {
			found = append(found, line)
		}
	}
	return found, nil
}

// FsckConnectivityOnly runs `git fsck --connectivity-only --no-dangling`
// against dir. Used as a defensive double-check after IndexPackStrict.
func FsckConnectivityOnly(ctx context.Context, dir string) error {
	_, err := run(ctx, dir, "fsck", "--connectivity-only", "--no-dangling", "--no-progress")
	return err
}
```

`UpdateRefDelete` and `UpdateRefCAS` were added to gitcli.go in Task 9; this task validates them via tests but does not redefine them.

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./internal/gitcli/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gitcli/gitcli.go internal/gitcli/gitcli_test.go
git commit -m "M3 gitcli: IndexPackStrict, RevListNotAll, FsckConnectivityOnly + ref update tests"
```

---

## Task 11: importer — extract BuildAndCommit for push reuse

**Files:**
- Modify: `internal/importer/importer.go`
- Create: `internal/importer/buildcommit_test.go` (or extend an existing test file)

M2's `Import` does: prepare local pack → upload pack → build .bvom/.bvcg locally → upload them → `repo.Create` → `repo.Commit`. The push flow is identical from "upload pack" onward, except it calls `repo.Open` instead of `repo.Create`, and it merges with the existing `manifest.Body` rather than starting from empty.

This task extracts the shared pipeline into a public function:

```go
func BuildAndCommit(
    ctx context.Context,
    store storage.ObjectStore,
    tenantID, repoID string,
    packPath string,                      // path to a local .pack with companion .idx
    refUpdates map[string]string,          // refname -> new OID; empty OID for delete
    base *manifest.Body,                   // current manifest body (nil for initial Import)
) (*manifest.Body, error)
```

- [ ] **Step 1: Write the failing test**

Create `internal/importer/buildcommit_test.go`:

```go
package importer_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/importer"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestBuildAndCommit_AppendsToExistingRepo(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	storeDir := t.TempDir()
	store, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// First Import: a single-commit bare repo.
	srcBare := filepath.Join(t.TempDir(), "src.git")
	work := filepath.Join(t.TempDir(), "wt")
	runOrFail(t, "", "git", "init", "--bare", srcBare)
	runOrFail(t, "", "git", "clone", srcBare, work)
	_ = os.WriteFile(filepath.Join(work, "a.txt"), []byte("hi\n"), 0o644)
	runOrFail(t, work, "git", "add", ".")
	runOrFail(t, work, "git", "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "init")
	runOrFail(t, work, "git", "push", "origin", "HEAD:refs/heads/main")
	if _, err := importer.Import(context.Background(), store, "acme", "demo", srcBare); err != nil {
		t.Fatalf("Import: %v", err)
	}

	// Add a second commit and produce a thin pack containing only the new objects.
	_ = os.WriteFile(filepath.Join(work, "b.txt"), []byte("more\n"), 0o644)
	runOrFail(t, work, "git", "add", ".")
	runOrFail(t, work, "git", "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "more")
	runOrFail(t, work, "git", "push", "origin", "HEAD:refs/heads/main")
	newOID := capture(t, work, "git", "rev-parse", "HEAD")

	tmpPack := filepath.Join(t.TempDir(), "delta")
	runOrFail(t, work, "bash", "-c", "git rev-list "+newOID+" --not --remotes | git pack-objects "+tmpPack)
	matches, _ := filepath.Glob(tmpPack + "-*.pack")
	if len(matches) != 1 {
		t.Fatalf("pack-objects produced %v", matches)
	}

	r, err := repo.Open(context.Background(), store, "acme", "demo")
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	view, err := r.ReadRoot(context.Background())
	if err != nil {
		t.Fatalf("ReadRoot: %v", err)
	}
	body := view.Body
	updates := map[string]string{"refs/heads/main": newOID}
	newBody, err := importer.BuildAndCommit(context.Background(), store, "acme", "demo", matches[0], updates, &body)
	if err != nil {
		t.Fatalf("BuildAndCommit: %v", err)
	}
	if newBody.Refs["refs/heads/main"] != newOID {
		t.Fatalf("refs not updated: %v", newBody.Refs)
	}
	if len(newBody.Packs) <= len(body.Packs) {
		t.Fatalf("packs not appended: before=%d after=%d", len(body.Packs), len(newBody.Packs))
	}
}

// shared helpers
func runOrFail(t *testing.T, dir, name string, args ...string) { /* implemented in importer_test.go pkg */ }
func capture(t *testing.T, dir, name string, args ...string) string { return "" }
```

The helpers `runOrFail` and `capture` should be defined in a shared test-only file. If `importer_test.go` already has them (or equivalents like `mustCmd`), drop the local stubs and use those. Otherwise add them to `internal/importer/buildcommit_test.go` directly with the obvious `exec.Command(...)` shape.

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/importer/...`
Expected: FAIL with `undefined: importer.BuildAndCommit`.

- [ ] **Step 3: Write the implementation**

Refactor `internal/importer/importer.go`:

```go
// BuildAndCommit shared pipeline used by both Import (initial) and the M3
// push handler (incremental). packPath points to a local .pack/.idx pair;
// refUpdates is a map of refname → new OID (empty OID = delete); base is
// the current manifest body (nil for initial import → empty body).
//
// On success returns the new *manifest.Body that was committed.
func BuildAndCommit(
	ctx context.Context,
	store storage.ObjectStore,
	tenantID, repoID string,
	packPath string,
	refUpdates map[string]string,
	base *manifest.Body,
) (*manifest.Body, error) {
	// 1. Upload pack + idx (content-addressed).
	packEntry, err := uploadPack(ctx, store, tenantID, repoID, packPath)
	if err != nil {
		return nil, fmt.Errorf("BuildAndCommit: upload pack: %w", err)
	}

	// 2. Build .bvom and .bvcg over (existing packs + new pack).
	allPacks := []manifest.PackEntry{}
	if base != nil {
		allPacks = append(allPacks, base.Packs...)
	}
	allPacks = append(allPacks, packEntry)

	bvomKey, bvomHash, err := buildAndUploadObjectMap(ctx, store, tenantID, repoID, allPacks)
	if err != nil {
		return nil, fmt.Errorf("BuildAndCommit: build object-map: %w", err)
	}
	bvcgKey, bvcgHash, err := buildAndUploadCommitGraph(ctx, store, tenantID, repoID, allPacks)
	if err != nil {
		return nil, fmt.Errorf("BuildAndCommit: build commit-graph: %w", err)
	}

	// 3. Construct the new manifest body.
	body := manifest.Body{
		DefaultBranch: defaultOr(base, "main"),
		Refs:          mergeRefs(base, refUpdates),
		Packs:         allPacks,
		Indexes: manifest.Indexes{
			ObjectMap:   manifest.IndexEntry{Key: bvomKey, Hash: bvomHash},
			CommitGraph: manifest.IndexEntry{Key: bvcgKey, Hash: bvcgHash},
		},
		Bundles: []manifest.BundleEntry{},
	}

	// 4. Commit via M1 transaction kernel.
	r, err := repo.Open(ctx, store, tenantID, repoID)
	if err != nil {
		return nil, fmt.Errorf("BuildAndCommit: repo.Open: %w", err)
	}
	if err := r.Commit(ctx, &body); err != nil {
		return nil, fmt.Errorf("BuildAndCommit: repo.Commit: %w", err)
	}
	return &body, nil
}

func defaultOr(b *manifest.Body, fallback string) string {
	if b != nil && b.DefaultBranch != "" {
		return b.DefaultBranch
	}
	return fallback
}

func mergeRefs(base *manifest.Body, updates map[string]string) map[string]string {
	out := map[string]string{}
	if base != nil {
		for k, v := range base.Refs {
			out[k] = v
		}
	}
	for ref, newOID := range updates {
		if newOID == "0000000000000000000000000000000000000000" || newOID == "" {
			delete(out, ref)
			continue
		}
		out[ref] = newOID
	}
	return out
}
```

Then refactor `Import` to call `BuildAndCommit` instead of duplicating the pipeline. The exact shape depends on the existing M2 internal helper names; if M2 has private functions named `uploadPack`, `buildAndUploadObjectMap`, `buildAndUploadCommitGraph`, they're reused as-is. If they don't exist with those names, this task is also where you give them those names by extracting them from the existing `Import` body.

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./internal/importer/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/importer/importer.go internal/importer/buildcommit_test.go
git commit -m "M3 importer: extract BuildAndCommit shared pipeline (Import + push)"
```

---

## Task 12: gateway — server skeleton, routing, options

**Files:**
- Create: `internal/gateway/server.go`
- Create: `internal/gateway/routes.go`
- Create: `internal/gateway/server_test.go`

`gateway.Server` implements `http.Handler`. Construction takes a store and an `Options` struct. Routes recognized:
- `GET  /healthz` → `200 OK`
- `GET  /` → text banner
- `GET  /{tenant}/{repo}.git/info/refs?service=...` → ref-advert dispatch (Tasks 14–15)
- `POST /{tenant}/{repo}.git/git-upload-pack` → upload-pack v2 dispatch (Task 15)
- `POST /{tenant}/{repo}.git/git-receive-pack` → receive-pack v0 dispatch (Tasks 16–17)
- anything else under `/{tenant}/{repo}.git/` → 404
- everything else → 404

Tenant and repo must match `^[A-Za-z0-9._-]+$`; otherwise 400.

- [ ] **Step 1: Write the failing test**

Create `internal/gateway/server_test.go`:

```go
package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	store, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	srv, err := NewServer(store, Options{
		MirrorDir: t.TempDir(),
		Version:   "0.1-test",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

func TestServer_Healthz(t *testing.T) {
	srv := newTestServer(t)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
}

func TestServer_Banner(t *testing.T) {
	srv := newTestServer(t)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

func TestServer_RejectsBadTenantOrRepo(t *testing.T) {
	srv := newTestServer(t)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	for _, path := range []string{
		"/.git/info/refs",
		"/foo/.git/info/refs",
		"/foo/with space.git/info/refs",
		"/..%2Fetc/x.git/info/refs",
	} {
		resp, err := http.Get(ts.URL + path + "?service=git-upload-pack")
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		if resp.StatusCode != 400 && resp.StatusCode != 404 {
			t.Fatalf("path %s: status %d, want 400 or 404", path, resp.StatusCode)
		}
	}
}

func TestServer_UnknownPathReturns404(t *testing.T) {
	srv := newTestServer(t)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	resp, _ := http.Get(ts.URL + "/foo/bar.git/info/wat")
	if resp.StatusCode != 404 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

// keep import alive
var _ = strings.NewReader
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/gateway/...`
Expected: FAIL with `undefined: NewServer`, etc.

- [ ] **Step 3: Write the implementation**

Create `internal/gateway/server.go`:

```go
// Package gateway implements the bucketvcs HTTP smart-Git server.
package gateway

import (
	"fmt"
	"net/http"
	"path"
	"regexp"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/mirror"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// Options configures a Server.
type Options struct {
	MirrorDir     string
	Version       string  // bucketvcs version string (used in agent= caps)
	AuthMode      AuthMode
	AuthToken     string
	MaxBodyBytes  int64
}

// AuthMode is defined in auth.go (Task 13).

// Server implements http.Handler.
type Server struct {
	store   storage.ObjectStore
	mgr     *mirror.Manager
	opts    Options
	mux     *http.ServeMux
}

// NewServer constructs a Server. The mirror manager acquires a process flock
// on opts.MirrorDir; the caller must Close() the server on shutdown.
func NewServer(store storage.ObjectStore, opts Options) (*Server, error) {
	if opts.Version == "" {
		opts.Version = "0.0-dev"
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = 1 << 30 // 1 GiB
	}
	mgr, err := mirror.NewManager(opts.MirrorDir, store)
	if err != nil {
		return nil, fmt.Errorf("gateway: mirror manager: %w", err)
	}
	s := &Server{store: store, mgr: mgr, opts: opts}
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("/healthz", s.handleHealthz)
	s.mux.HandleFunc("/", s.routeRoot)
	return s, nil
}

func (s *Server) Close() error { return s.mgr.Close() }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) routeRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintf(w, "bucketvcs %s\n", s.opts.Version)
		return
	}
	s.routeRepo(w, r)
}
```

Create `internal/gateway/routes.go`:

```go
package gateway

import (
	"net/http"
	"path"
	"regexp"
	"strings"
)

var nameRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// routeRepo dispatches /{tenant}/{repo}.git/<sub-path>.
func (s *Server) routeRepo(w http.ResponseWriter, r *http.Request) {
	clean := path.Clean(r.URL.Path)
	parts := strings.SplitN(strings.TrimPrefix(clean, "/"), "/", 3)
	if len(parts) < 3 {
		http.NotFound(w, r)
		return
	}
	tenant := parts[0]
	repoSeg := parts[1]
	rest := parts[2]
	if !strings.HasSuffix(repoSeg, ".git") {
		http.NotFound(w, r)
		return
	}
	repoID := strings.TrimSuffix(repoSeg, ".git")
	if !nameRE.MatchString(tenant) || !nameRE.MatchString(repoID) {
		http.Error(w, "invalid tenant or repository name", http.StatusBadRequest)
		return
	}

	switch {
	case r.Method == http.MethodGet && rest == "info/refs":
		s.handleInfoRefs(w, r, tenant, repoID)
	case r.Method == http.MethodPost && rest == "git-upload-pack":
		s.handleUploadPack(w, r, tenant, repoID)
	case r.Method == http.MethodPost && rest == "git-receive-pack":
		s.handleReceivePack(w, r, tenant, repoID)
	default:
		http.NotFound(w, r)
	}
}

// Stubs for Tasks 14–17. Each task replaces these with a real handler.
func (s *Server) handleInfoRefs(w http.ResponseWriter, r *http.Request, tenant, repoID string) {
	http.Error(w, "info/refs not yet implemented", http.StatusNotImplemented)
}
func (s *Server) handleUploadPack(w http.ResponseWriter, r *http.Request, tenant, repoID string) {
	http.Error(w, "upload-pack not yet implemented", http.StatusNotImplemented)
}
func (s *Server) handleReceivePack(w http.ResponseWriter, r *http.Request, tenant, repoID string) {
	http.Error(w, "receive-pack not yet implemented", http.StatusNotImplemented)
}
```

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./internal/gateway/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gateway/server.go internal/gateway/routes.go internal/gateway/server_test.go
git commit -m "M3 gateway: server skeleton + routing for healthz/info-refs/upload-pack/receive-pack"
```

---

## Task 13: gateway — auth middleware

**Files:**
- Create: `internal/gateway/auth.go`
- Create: `internal/gateway/auth_test.go`
- Modify: `internal/gateway/routes.go` (wrap protocol handlers in auth middleware)

Three modes set at server start: `AuthAnonymous`, `AuthWriteOnly`, `AuthAll`. Wire format: HTTP Basic with username `bucketvcs` and password = configured token. Constant-time compare on the password. Read vs write classification: `service=git-upload-pack` query param or `git-upload-pack` URL suffix → read; receive equivalents → write.

- [ ] **Step 1: Write the failing test**

Create `internal/gateway/auth_test.go`:

```go
package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func newTestServerWithAuth(t *testing.T, mode AuthMode, token string) *httptest.Server {
	t.Helper()
	store, _ := localfs.Open(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	srv, err := NewServer(store, Options{
		MirrorDir: t.TempDir(),
		Version:   "test",
		AuthMode:  mode,
		AuthToken: token,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

func TestAuth_AnonymousMode_AllowsBoth(t *testing.T) {
	ts := newTestServerWithAuth(t, AuthAnonymous, "")
	for _, svc := range []string{"git-upload-pack", "git-receive-pack"} {
		resp, err := http.Get(ts.URL + "/acme/demo.git/info/refs?service=" + svc)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		// 404 (repo not found) is fine — we just want != 401.
		if resp.StatusCode == 401 {
			t.Fatalf("svc=%s: got 401 in anonymous mode", svc)
		}
	}
}

func TestAuth_WriteOnlyMode_AllowsAnonRead_RejectsAnonWrite(t *testing.T) {
	ts := newTestServerWithAuth(t, AuthWriteOnly, "secret")
	resp, _ := http.Get(ts.URL + "/acme/demo.git/info/refs?service=git-upload-pack")
	if resp.StatusCode == 401 {
		t.Fatalf("anon read in write-only mode: 401")
	}
	resp, _ = http.Get(ts.URL + "/acme/demo.git/info/refs?service=git-receive-pack")
	if resp.StatusCode != 401 {
		t.Fatalf("anon write in write-only mode: got %d, want 401", resp.StatusCode)
	}
}

func TestAuth_AllMode_RequiresTokenForBoth(t *testing.T) {
	ts := newTestServerWithAuth(t, AuthAll, "secret")
	for _, svc := range []string{"git-upload-pack", "git-receive-pack"} {
		resp, _ := http.Get(ts.URL + "/acme/demo.git/info/refs?service=" + svc)
		if resp.StatusCode != 401 {
			t.Fatalf("svc=%s anon: got %d, want 401", svc, resp.StatusCode)
		}
	}
	// With correct credentials.
	for _, svc := range []string{"git-upload-pack", "git-receive-pack"} {
		req, _ := http.NewRequest("GET", ts.URL+"/acme/demo.git/info/refs?service="+svc, nil)
		req.SetBasicAuth("bucketvcs", "secret")
		resp, _ := http.DefaultClient.Do(req)
		if resp.StatusCode == 401 {
			t.Fatalf("svc=%s with token: 401", svc)
		}
	}
}

func TestAuth_AllMode_RejectsWrongToken(t *testing.T) {
	ts := newTestServerWithAuth(t, AuthAll, "secret")
	req, _ := http.NewRequest("GET", ts.URL+"/acme/demo.git/info/refs?service=git-upload-pack", nil)
	req.SetBasicAuth("bucketvcs", "wrong")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Fatalf("wrong token: got %d, want 401", resp.StatusCode)
	}
}

var _ = strings.NewReader
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/gateway/...`
Expected: FAIL with `undefined: AuthAnonymous`, etc.

- [ ] **Step 3: Write the implementation**

Create `internal/gateway/auth.go`:

```go
package gateway

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// AuthMode controls which requests require authentication.
type AuthMode int

const (
	// AuthAnonymous accepts all requests without authentication.
	AuthAnonymous AuthMode = iota
	// AuthWriteOnly accepts read requests anonymously; write requests require
	// the configured token.
	AuthWriteOnly
	// AuthAll requires the configured token for every request.
	AuthAll
)

const authRealm = `Basic realm="bucketvcs"`
const authUser = "bucketvcs"

// classify determines whether a request is a read or a write based on the URL
// path and the "service" query parameter.
func classify(r *http.Request) (isWrite bool) {
	if strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
		return true
	}
	if strings.HasSuffix(r.URL.Path, "/info/refs") {
		return r.URL.Query().Get("service") == "git-receive-pack"
	}
	return false
}

// authorize returns true if the request should proceed; if false, it has
// already written a 401.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) bool {
	switch s.opts.AuthMode {
	case AuthAnonymous:
		return true
	case AuthWriteOnly:
		if !classify(r) {
			return true
		}
	case AuthAll:
		// fallthrough to credential check
	}
	user, pass, ok := r.BasicAuth()
	if !ok || user != authUser ||
		subtle.ConstantTimeCompare([]byte(pass), []byte(s.opts.AuthToken)) != 1 {
		w.Header().Set("WWW-Authenticate", authRealm)
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return false
	}
	return true
}
```

Update `internal/gateway/routes.go` to call `authorize` at the top of each protocol handler dispatch:

```go
case r.Method == http.MethodGet && rest == "info/refs":
	if !s.authorize(w, r) {
		return
	}
	s.handleInfoRefs(w, r, tenant, repoID)
case r.Method == http.MethodPost && rest == "git-upload-pack":
	if !s.authorize(w, r) {
		return
	}
	s.handleUploadPack(w, r, tenant, repoID)
case r.Method == http.MethodPost && rest == "git-receive-pack":
	if !s.authorize(w, r) {
		return
	}
	s.handleReceivePack(w, r, tenant, repoID)
```

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./internal/gateway/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gateway/auth.go internal/gateway/auth_test.go internal/gateway/routes.go
git commit -m "M3 gateway: HTTP Basic auth middleware with anonymous/write-only/all modes"
```

---

## Task 14: gateway — info/refs handler

**Files:**
- Create: `internal/gateway/inforefs.go`
- Create: `internal/gateway/inforefs_test.go`
- Modify: `internal/gateway/routes.go` (replace handleInfoRefs stub with real call)

`GET /info/refs?service=git-upload-pack` → if `Git-Protocol: version=2` header present, write the v2 capability advertisement (Task 3); otherwise write a v0-shaped one with a clean error message inside.

`GET /info/refs?service=git-receive-pack` → write a v0 ref-advertisement: `<oid> <ref>\0<caps>` for the first line, `<oid> <ref>` for subsequent lines, terminated by flush.

For receive-pack, capabilities advertised: `report-status delete-refs ofs-delta atomic side-band-64k agent=bucketvcs/<v>`.

The Content-Type per the smart-HTTP spec is `application/x-git-{service}-advertisement`.

- [ ] **Step 1: Write the failing test**

Create `internal/gateway/inforefs_test.go`:

```go
package gateway

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/importer"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// makeRepo imports a tiny repo into the given store.
func makeRepoInStore(t *testing.T, storeDir, tenant, repoID string) {
	t.Helper()
	srcBare := t.TempDir() + "/src.git"
	work := t.TempDir() + "/wt"
	mustExec(t, "", "git", "init", "--bare", srcBare)
	mustExec(t, "", "git", "clone", srcBare, work)
	mustExec(t, work, "bash", "-c", "echo hi > a.txt && git add . && git -c user.email=t@t -c user.name=t commit -m init && git push origin HEAD:refs/heads/main")
	store, _ := localfs.Open(storeDir)
	defer store.Close()
	if _, err := importer.Import(context.Background(), store, tenant, repoID, srcBare); err != nil {
		t.Fatalf("Import: %v", err)
	}
}

func TestInfoRefs_V2UploadPack(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	storeDir := t.TempDir()
	makeRepoInStore(t, storeDir, "acme", "demo")
	store, _ := localfs.Open(storeDir)
	t.Cleanup(func() { _ = store.Close() })
	srv, err := NewServer(store, Options{MirrorDir: t.TempDir(), Version: "test"})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest("GET", ts.URL+"/acme/demo.git/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Git-Protocol", "version=2")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/x-git-upload-pack-advertisement" {
		t.Fatalf("Content-Type: %q", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("version 2")) {
		t.Fatalf("body missing 'version 2': %q", body)
	}
	if !bytes.Contains(body, []byte("ls-refs=unborn")) {
		t.Fatalf("body missing 'ls-refs=unborn': %q", body)
	}
}

func TestInfoRefs_ReceivePackV0(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	storeDir := t.TempDir()
	makeRepoInStore(t, storeDir, "acme", "demo")
	store, _ := localfs.Open(storeDir)
	t.Cleanup(func() { _ = store.Close() })
	srv, _ := NewServer(store, Options{MirrorDir: t.TempDir(), Version: "test"})
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/acme/demo.git/info/refs?service=git-receive-pack")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/x-git-receive-pack-advertisement" {
		t.Fatalf("Content-Type: %q", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("# service=git-receive-pack")) {
		t.Fatalf("missing service header: %q", body)
	}
	if !bytes.Contains(body, []byte("report-status")) {
		t.Fatalf("missing capability: %q", body)
	}
	if !bytes.Contains(body, []byte("refs/heads/main")) {
		t.Fatalf("missing ref: %q", body)
	}
}

func TestInfoRefs_RejectsUnknownService(t *testing.T) {
	srv, _ := NewServer(mustOpenStore(t, t.TempDir()), Options{MirrorDir: t.TempDir()})
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	resp, _ := http.Get(ts.URL + "/acme/demo.git/info/refs?service=git-evil-pack")
	if resp.StatusCode != 400 {
		t.Fatalf("unknown service: status %d, want 400", resp.StatusCode)
	}
}

// helpers
func mustExec(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	out, err := runShell(dir, name, args...)
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
}

func mustOpenStore(t *testing.T, dir string) *localfs.Store {
	t.Helper()
	store, err := localfs.Open(dir)
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

var _ = strings.NewReader
```

Add a helper `internal/gateway/runshell_test.go`:

```go
package gateway

import (
	"bytes"
	"os/exec"
)

func runShell(dir, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.Bytes(), err
}
```

If `*localfs.Store` is unexported in the M0 package, replace the test helper return type with `storage.ObjectStore` and adjust accordingly.

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/gateway/...`
Expected: FAIL — handler still 501.

- [ ] **Step 3: Write the implementation**

Create `internal/gateway/inforefs.go`:

```go
package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/pktline"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/v2proto"
)

func (s *Server) handleInfoRefs(w http.ResponseWriter, r *http.Request, tenant, repoID string) {
	service := r.URL.Query().Get("service")
	switch service {
	case "git-upload-pack", "git-receive-pack":
	default:
		http.Error(w, "unknown service", http.StatusBadRequest)
		return
	}

	r2, err := repo.Open(r.Context(), s.store, tenant, repoID)
	if err != nil {
		http.Error(w, "repository not found", http.StatusNotFound)
		return
	}
	view, err := r2.ReadRoot(r.Context())
	if err != nil {
		http.Error(w, "internal storage error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/x-git-"+service+"-advertisement")
	w.Header().Set("Cache-Control", "no-cache")

	if service == "git-upload-pack" {
		// v2 advertisement when the client opted in via header.
		if r.Header.Get("Git-Protocol") == "version=2" {
			if err := v2proto.WriteV2Advertisement(w, service, s.opts.Version); err != nil {
				return
			}
			return
		}
		// Fallback v0 ref-advertisement with a hint. We still emit the
		// service header + ref list so old git binaries see something
		// recognizable; M3's actual upload-pack POST will demand v2.
		writeV0Advertisement(w, service, &view, s.opts.Version, false)
		return
	}
	// receive-pack — always v0
	writeV0Advertisement(w, service, &view, s.opts.Version, true)
}

// writeV0Advertisement emits the smart-HTTP v0 layout:
//   "# service=<service>\n"
//   flush
//   <oid> <ref>\0<caps>\n
//   <oid> <ref>\n ...
//   flush
//
// For an empty repo (no refs), we emit the "0000000...0 capabilities^{}\0<caps>\n"
// canonical empty-advertisement line per smart-HTTP spec.
func writeV0Advertisement(w io.Writer, service string, view *repo.RootView, version string, isReceivePack bool) {
	pw := pktline.NewWriter(w)
	_ = pw.WriteString("# service=" + service + "\n")
	_ = pw.WriteFlush()

	caps := uploadPackV0Caps(version)
	if isReceivePack {
		caps = receivePackV0Caps(version)
	}

	body := view.Body
	names := make([]string, 0, len(body.Refs))
	for n := range body.Refs {
		names = append(names, n)
	}
	sort.Strings(names)

	if len(names) == 0 {
		// Empty advertisement.
		_ = pw.WriteString("0000000000000000000000000000000000000000 capabilities^{}\x00" + caps + "\n")
		_ = pw.WriteFlush()
		return
	}

	first := true
	for _, n := range names {
		oid := body.Refs[n]
		if first {
			_ = pw.WriteString(oid + " " + n + "\x00" + caps + "\n")
			first = false
			continue
		}
		_ = pw.WriteString(oid + " " + n + "\n")
	}
	_ = pw.WriteFlush()
}

func uploadPackV0Caps(version string) string {
	return strings.Join([]string{
		"multi_ack_detailed",
		"no-done",
		"side-band-64k",
		"thin-pack",
		"ofs-delta",
		"agent=" + v2proto.AgentName + "/" + version,
		"object-format=sha1",
	}, " ")
}

func receivePackV0Caps(version string) string {
	return strings.Join([]string{
		"report-status",
		"delete-refs",
		"ofs-delta",
		"atomic",
		"side-band-64k",
		"agent=" + v2proto.AgentName + "/" + version,
		"object-format=sha1",
	}, " ")
}

// The fmt import is used only in error paths; keep it referenced.
var _ = fmt.Sprintf

// suppress context import warning if unused
var _ = context.Background
```

Update `routes.go` to delete the handleInfoRefs stub now that the real handler is in inforefs.go (the method on *Server satisfies the same dispatch).

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./internal/gateway/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gateway/inforefs.go internal/gateway/inforefs_test.go internal/gateway/runshell_test.go internal/gateway/routes.go
git commit -m "M3 gateway: info/refs handler (v2 upload-pack advert + v0 receive-pack advert)"
```

---

## Task 15: gateway — POST /git-upload-pack handler

**Files:**
- Create: `internal/gateway/upload_pack.go`
- Create: `internal/gateway/upload_pack_test.go`

The POST handler reads the v2 command stream, dispatches to `v2proto.HandleLsRefs` for `command=ls-refs` or `v2proto.ParseFetchArgs` + the fetch flow for `command=fetch`. The fetch flow:
1. Acquire mirror RLock (lazy materialize / sync).
2. Validate every `want` resolves in the mirror.
3. `gitcli.PackObjectsForFetch` → returns an `io.ReadCloser` over the pack.
4. Write `WriteAcknowledgments` if any haves were sent.
5. Write `packfile\n` + delim, then side-band the pack bytes on band 1.
6. Write flush.

Content-Type: `application/x-git-upload-pack-result`.

- [ ] **Step 1: Write the failing test**

Create `internal/gateway/upload_pack_test.go`:

```go
package gateway

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestUploadPack_GitCloneEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	storeDir := t.TempDir()
	makeRepoInStore(t, storeDir, "acme", "demo")
	store, _ := localfs.Open(storeDir)
	t.Cleanup(func() { _ = store.Close() })
	srv, _ := NewServer(store, Options{MirrorDir: t.TempDir(), Version: "test"})
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	dst := t.TempDir() + "/clone"
	cmd := exec.Command("git", "clone", "-c", "protocol.version=2", ts.URL+"/acme/demo.git", dst)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git clone: %v\n%s", err, out)
	}
	if _, err := io.ReadFile(dst + "/a.txt"); err != nil {
		t.Fatalf("expected a.txt in clone: %v", err)
	}
}

// minimal smoke: lsrefs returns expected ref over v2
func TestUploadPack_LsRefsOverV2(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	storeDir := t.TempDir()
	makeRepoInStore(t, storeDir, "acme", "demo")
	store, _ := localfs.Open(storeDir)
	t.Cleanup(func() { _ = store.Close() })
	srv, _ := NewServer(store, Options{MirrorDir: t.TempDir(), Version: "test"})
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	body := bytes.NewBufferString("0014command=ls-refs\n0001000fpeel\n0009symrefs\n0019ref-prefix refs/heads/\n0000")
	req, _ := http.NewRequest("POST", ts.URL+"/acme/demo.git/git-upload-pack", body)
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Set("Git-Protocol", "version=2")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(got, []byte("refs/heads/main")) {
		t.Fatalf("ls-refs response missing main: %q", got)
	}
}

// To shim io.ReadFile in older toolchains:
var _ = strings.NewReader
```

`io.ReadFile` doesn't exist in stdlib; replace with `os.ReadFile`. Adjust the import accordingly.

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/gateway/...`
Expected: FAIL.

- [ ] **Step 3: Write the implementation**

Create `internal/gateway/upload_pack.go`:

```go
package gateway

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
	"github.com/bucketvcs/bucketvcs/internal/pktline"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/v2proto"
)

func (s *Server) handleUploadPack(w http.ResponseWriter, r *http.Request, tenant, repoID string) {
	if r.Header.Get("Git-Protocol") != "version=2" {
		http.Error(w, "protocol v2 required (Git-Protocol: version=2)", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()
	body := http.MaxBytesReader(w, r.Body, s.opts.MaxBodyBytes)
	tokens, err := drainPktLine(body)
	if err != nil {
		http.Error(w, "bad request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(tokens) == 0 || tokens[0].Type != pktline.Data {
		http.Error(w, "empty command", http.StatusBadRequest)
		return
	}
	cmdLine := strings.TrimRight(string(tokens[0].Payload), "\n")
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")

	r2, err := repo.Open(r.Context(), s.store, tenant, repoID)
	if err != nil {
		http.Error(w, "repository not found", http.StatusNotFound)
		return
	}
	view, err := r2.ReadRoot(r.Context())
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	switch cmdLine {
	case "command=ls-refs":
		if err := v2proto.HandleLsRefs(tokens, &view.Body, w); err != nil {
			http.Error(w, "ls-refs: "+err.Error(), http.StatusBadRequest)
		}
	case "command=fetch":
		s.handleFetch(w, r, tenant, repoID, tokens, &view.Body)
	default:
		http.Error(w, "unsupported command "+cmdLine, http.StatusBadRequest)
	}
}

func (s *Server) handleFetch(w http.ResponseWriter, r *http.Request, tenant, repoID string, tokens []pktline.Token, body *manifestBody) {
	req, err := v2proto.ParseFetchArgs(tokens)
	if err != nil {
		http.Error(w, "fetch: "+err.Error(), http.StatusBadRequest)
		return
	}
	m, err := s.mgr.Open(r.Context(), tenant, repoID)
	if err != nil {
		http.Error(w, "mirror: "+err.Error(), http.StatusInternalServerError)
		return
	}
	m.RLock()
	defer m.RUnlock()

	// Resolve want-refs to OIDs through the manifest body.
	allWants := append([]string{}, req.Wants...)
	for _, ref := range req.WantRefs {
		oid, ok := body.Refs[ref]
		if !ok {
			http.Error(w, "fetch: unknown ref "+ref, http.StatusBadRequest)
			return
		}
		allWants = append(allWants, oid)
	}
	for _, oid := range allWants {
		if _, err := gitcli.RevParseObjectKind(r.Context(), m.BareDir(), oid); err != nil {
			http.Error(w, "fetch: not our ref "+oid, http.StatusBadRequest)
			return
		}
	}

	pw := pktline.NewWriter(w)
	if len(req.Haves) > 0 {
		// Compute "common" haves: those that exist in our mirror.
		var commons []string
		for _, h := range req.Haves {
			if _, err := gitcli.RevParseObjectKind(r.Context(), m.BareDir(), h); err == nil {
				commons = append(commons, h)
			}
		}
		if err := v2proto.WriteAcknowledgments(w, commons, nil); err != nil {
			return
		}
		if !req.Done {
			// Tell client to send more rounds. M3 always sets ready/NAK above
			// and proceeds to packfile when 'done' is present; if not, we end.
			_ = pw.WriteFlush()
			return
		}
	}
	if err := pw.WriteString("packfile\n"); err != nil {
		return
	}
	if err := pw.WriteDelim(); err != nil {
		return
	}

	pack, err := gitcli.PackObjectsForFetch(r.Context(), m.BareDir(), gitcli.PackForFetchOptions{
		Wants:      allWants,
		Haves:      req.Haves,
		ThinPack:   req.ThinPack,
		IncludeTag: req.IncludeTag,
		OfsDelta:   req.OfsDelta,
		NoProgress: req.NoProgress,
		Depth:      req.Depth,
	})
	if err != nil {
		http.Error(w, "pack-objects: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer pack.Close()

	sb := pktline.NewSidebandWriter(pw)
	buf := make([]byte, 65000)
	for {
		n, rerr := pack.Read(buf)
		if n > 0 {
			if _, werr := sb.WriteData(buf[:n]); werr != nil {
				return
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			_, _ = sb.WriteFatal([]byte("pack stream: " + rerr.Error()))
			return
		}
	}
	_ = pw.WriteFlush()
}

// drainPktLine reads all tokens until EOF.
func drainPktLine(r io.Reader) ([]pktline.Token, error) {
	pr := pktline.NewReader(r)
	var out []pktline.Token
	for {
		tok, err := pr.Read()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("pktline: %w", err)
		}
		// Copy payload because pr reuses the buffer.
		cp := append([]byte{}, tok.Payload...)
		out = append(out, pktline.Token{Type: tok.Type, Payload: cp})
	}
}

// manifestBody is a local alias to avoid an import cycle if any.
type manifestBody = struct {
	DefaultBranch string
	Refs          map[string]string
	Packs         []interface{} // not used in handleFetch
	Indexes       interface{}   // ditto
	Bundles       []interface{}
}
```

The local `manifestBody` alias is wrong (it doesn't match the real struct). Replace with the actual import:

```go
import "github.com/bucketvcs/bucketvcs/internal/repo/manifest"
```

and change `*manifestBody` → `*manifest.Body`. Delete the local alias.

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./internal/gateway/...`
Expected: PASS, including the end-to-end `git clone` test.

- [ ] **Step 5: Commit**

```bash
git add internal/gateway/upload_pack.go internal/gateway/upload_pack_test.go
git commit -m "M3 gateway: POST /git-upload-pack handler (v2 ls-refs + fetch with side-band pack)"
```

---

## Task 16: gateway — POST /git-receive-pack: parse update commands and pack

**Files:**
- Create: `internal/gateway/receive_pack.go`
- Create: `internal/gateway/receive_pack_test.go`

This task covers the *parsing* half: read the v0 update-command pkt-lines, capture capability flags from the first line, then drain the remaining body into a temp pack file. The *validate-and-commit* half is Task 17.

The handler:
1. Reads pkt-line until flush; captures `(old, new, ref)` tuples and capability set.
2. Detects delete-only push (no PACK header expected after flush) and skips pack copy.
3. Otherwise reads remaining body bytes into `<mirror>/incoming/<request-id>.pack`.
4. Returns the parsed structures to Task 17's logic.

For this task the handler stops at "we have a temp pack and a list of updates"; it then writes a placeholder report and 200. Task 17 fills in the validation and commit, and replaces the placeholder report with real status.

- [ ] **Step 1: Write the failing test**

Create `internal/gateway/receive_pack_test.go`:

```go
package gateway

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestReceivePack_ParsesUpdateCommands(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	storeDir := t.TempDir()
	makeRepoInStore(t, storeDir, "acme", "demo")
	store, _ := localfs.Open(storeDir)
	t.Cleanup(func() { _ = store.Close() })
	srv, _ := NewServer(store, Options{MirrorDir: t.TempDir(), Version: "test"})
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	// Construct a minimal "delete-only" receive-pack body. No PACK section.
	const oldOID = "1111111111111111111111111111111111111111"
	const nullOID = "0000000000000000000000000000000000000000"
	body := &bytes.Buffer{}
	line := oldOID + " " + nullOID + " refs/heads/feature\x00report-status delete-refs"
	pktWrite(body, line+"\n")
	body.WriteString("0000")

	req, _ := http.NewRequest("POST", ts.URL+"/acme/demo.git/git-receive-pack", body)
	req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

func TestReceivePack_RejectsBadRefName(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	storeDir := t.TempDir()
	makeRepoInStore(t, storeDir, "acme", "demo")
	store, _ := localfs.Open(storeDir)
	t.Cleanup(func() { _ = store.Close() })
	srv, _ := NewServer(store, Options{MirrorDir: t.TempDir(), Version: "test"})
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	const oldOID = "1111111111111111111111111111111111111111"
	const newOID = "2222222222222222222222222222222222222222"
	body := &bytes.Buffer{}
	pktWrite(body, oldOID+" "+newOID+" refs/heads/bad ref\x00report-status\n")
	body.WriteString("0000")

	req, _ := http.NewRequest("POST", ts.URL+"/acme/demo.git/git-receive-pack", body)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 400 {
		t.Fatalf("bad ref name: status %d, want 400", resp.StatusCode)
	}
}

// pktWrite writes one Data frame with payload p.
func pktWrite(buf *bytes.Buffer, p string) {
	n := len(p) + 4
	for i := 12; i >= 0; i -= 4 {
		nibble := (n >> i) & 0xf
		var c byte
		if nibble < 10 {
			c = '0' + byte(nibble)
		} else {
			c = 'a' + byte(nibble-10)
		}
		buf.WriteByte(c)
	}
	buf.WriteString(p)
}

var _ = strings.NewReader
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/gateway/...`
Expected: FAIL — handler still 501.

- [ ] **Step 3: Write the implementation**

Create `internal/gateway/receive_pack.go`:

```go
package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
	"github.com/bucketvcs/bucketvcs/internal/pktline"
)

const refValid = `refs/heads/|refs/tags/|refs/notes/`

type updateCommand struct {
	OldOID  string
	NewOID  string
	Refname string
}

type receivePackRequest struct {
	Caps        map[string]bool
	Updates     []updateCommand
	PackPath    string // empty for delete-only push
	IsAtomic    bool
}

func (s *Server) handleReceivePack(w http.ResponseWriter, r *http.Request, tenant, repoID string) {
	defer r.Body.Close()
	body := http.MaxBytesReader(w, r.Body, s.opts.MaxBodyBytes)

	mgr := s.mgr
	m, err := mgr.Open(r.Context(), tenant, repoID)
	if err != nil {
		http.Error(w, "mirror: "+err.Error(), http.StatusInternalServerError)
		return
	}

	req, err := parseReceivePackRequest(body, m.IncomingDir())
	if err != nil {
		if req != nil && req.PackPath != "" {
			_ = os.Remove(req.PackPath)
		}
		http.Error(w, "receive-pack: "+err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")

	// Task 17 replaces this block with full validation + commit.
	// For this task we only emit a placeholder report.
	if err := writeReportPlaceholder(w, req); err != nil {
		// best-effort
		return
	}

	// Cleanup transient pack regardless of outcome until Task 17 wires it in.
	if req.PackPath != "" {
		_ = os.Remove(req.PackPath)
		_ = os.Remove(strings.TrimSuffix(req.PackPath, ".pack") + ".idx")
	}
}

func parseReceivePackRequest(body io.Reader, incoming string) (*receivePackRequest, error) {
	pr := pktline.NewReader(body)
	req := &receivePackRequest{Caps: map[string]bool{}}
	first := true
	for {
		tok, err := pr.Read()
		if err == io.EOF {
			return nil, errors.New("unexpected EOF before flush")
		}
		if err != nil {
			return nil, fmt.Errorf("read commands: %w", err)
		}
		if tok.Type == pktline.Flush {
			break
		}
		if tok.Type != pktline.Data {
			return nil, fmt.Errorf("unexpected token %v", tok.Type)
		}
		line := strings.TrimRight(string(tok.Payload), "\n")
		if first {
			first = false
			if i := strings.IndexByte(line, '\x00'); i >= 0 {
				caps := strings.Fields(line[i+1:])
				for _, c := range caps {
					req.Caps[c] = true
				}
				line = line[:i]
			}
			if req.Caps["atomic"] {
				req.IsAtomic = true
			}
		}
		// "<old> <new> <refname>"
		parts := strings.SplitN(line, " ", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("malformed update command %q", line)
		}
		old, neu, ref := parts[0], parts[1], parts[2]
		if !validHexOID(old) || !validHexOID(neu) {
			return nil, fmt.Errorf("invalid OID in command %q", line)
		}
		if err := validateRefname(ref); err != nil {
			return nil, err
		}
		if strings.HasPrefix(ref, "refs/replace/") {
			return nil, fmt.Errorf("refs/replace/* writes are not allowed")
		}
		req.Updates = append(req.Updates, updateCommand{OldOID: old, NewOID: neu, Refname: ref})
	}
	if len(req.Updates) == 0 {
		return nil, errors.New("no update commands")
	}

	allDelete := true
	for _, u := range req.Updates {
		if u.NewOID != "0000000000000000000000000000000000000000" {
			allDelete = false
			break
		}
	}
	if !allDelete {
		// Read remaining body into a temp pack file under <mirror>/incoming/.
		if err := os.MkdirAll(incoming, 0o755); err != nil {
			return req, fmt.Errorf("incoming mkdir: %w", err)
		}
		idBytes := make([]byte, 12)
		_, _ = rand.Read(idBytes)
		packPath := filepath.Join(incoming, "rcv-"+hex.EncodeToString(idBytes)+".pack")
		f, err := os.OpenFile(packPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
		if err != nil {
			return req, fmt.Errorf("create incoming: %w", err)
		}
		if _, err := io.Copy(f, body); err != nil {
			_ = f.Close()
			_ = os.Remove(packPath)
			return req, fmt.Errorf("write incoming: %w", err)
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(packPath)
			return req, fmt.Errorf("close incoming: %w", err)
		}
		req.PackPath = packPath
	}
	return req, nil
}

func validHexOID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !(('0' <= c && c <= '9') || ('a' <= c && c <= 'f') || ('A' <= c && c <= 'F')) {
			return false
		}
	}
	return true
}

func validateRefname(name string) error {
	// Defer to git for canonical validation in non-test paths; this is a
	// pre-filter to refuse trivially-bad shapes without a fork.
	if name == "" || strings.ContainsAny(name, " \t\x00\\:?*[") {
		return fmt.Errorf("invalid refname %q", name)
	}
	if strings.HasPrefix(name, "-") || strings.Contains(name, "..") {
		return fmt.Errorf("invalid refname %q", name)
	}
	return gitcli.CheckRefFormat(nil, name)
}

func writeReportPlaceholder(w io.Writer, req *receivePackRequest) error {
	pw := pktline.NewWriter(w)
	if err := pw.WriteString("unpack ok\n"); err != nil {
		return err
	}
	for _, u := range req.Updates {
		if err := pw.WriteString("ok " + u.Refname + "\n"); err != nil {
			return err
		}
	}
	return pw.WriteFlush()
}
```

`gitcli.CheckRefFormat` already takes a `context.Context` from M2; pass `r.Context()` instead of `nil` if the test path tolerates it. The plan uses `nil` here for brevity; the implementer should thread context through if M2's signature requires non-nil. Update accordingly.

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./internal/gateway/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gateway/receive_pack.go internal/gateway/receive_pack_test.go
git commit -m "M3 gateway: receive-pack request parser (commands + caps + incoming pack stage)"
```

---

## Task 17: gateway — POST /git-receive-pack: validate, commit, IngestPack

**Files:**
- Modify: `internal/gateway/receive_pack.go`
- Modify: `internal/gateway/receive_pack_test.go` (add end-to-end push tests)

This task replaces the placeholder report path with the full push flow:

1. Acquire `mirror.Lock()`.
2. Sync mirror (Section 7.4).
3. Validate `old-oid` for each command via `git rev-parse <ref>` against the mirror.
4. If pack present: `gitcli.IndexPackStrict(ctx, m.BareDir(), packPath)` → produces `.idx`.
5. If pack present: connectivity check via `gitcli.RevListNotAll(ctx, m.BareDir(), newOIDs)` → must be empty (after pack-objects materialized into the mirror's odb area).
6. Build `manifest.Body` updates: convert `[]updateCommand` to `map[string]string`.
7. Call `importer.BuildAndCommit(ctx, store, tenant, repo, packPath, updates, &currentBody)`.
8. Call `mirror.IngestPack(ctx, packPath, refUpdates, newVersion)` to bring local mirror in sync.
9. Emit per-ref `ok`/`ng` status report.

Side-band wraps the whole report on band 1 if client advertised `side-band-64k`.

- [ ] **Step 1: Write the failing test**

Append to `internal/gateway/receive_pack_test.go`:

```go
func TestReceivePack_GitPushEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	// Set up: empty bucket repo, gateway running, and a populated source bare repo.
	storeDir := t.TempDir()
	store, _ := localfs.Open(storeDir)
	t.Cleanup(func() { _ = store.Close() })
	if _, err := repoCreate(context.Background(), store, "acme", "demo"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	srv, _ := NewServer(store, Options{MirrorDir: t.TempDir(), Version: "test"})
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	src := t.TempDir() + "/src.git"
	work := t.TempDir() + "/wt"
	mustExec(t, "", "git", "init", "--bare", src)
	mustExec(t, "", "git", "clone", src, work)
	mustExec(t, work, "bash", "-c", "echo hello > a.txt && git add . && git -c user.email=t@t -c user.name=t commit -m init")
	cmd := exec.Command("git", "-C", work, "push", ts.URL+"/acme/demo.git", "HEAD:refs/heads/main")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git push: %v\n%s", err, out)
	}

	// Verify ref now lives in manifest body.
	r2, _ := repo.Open(context.Background(), store, "acme", "demo")
	view, _ := r2.ReadRoot(context.Background())
	if _, ok := view.Body.Refs["refs/heads/main"]; !ok {
		t.Fatalf("expected refs/heads/main in manifest after push: %+v", view.Body.Refs)
	}
}

// repoCreate dispatches to repo.Create with default opts.
func repoCreate(ctx context.Context, store storage.ObjectStore, tenant, repoID string) (*repo.Repo, error) {
	return repo.Create(ctx, store, tenant, repoID, repo.CreateOptions{})
}
```

Add necessary imports: `context`, `os/exec`, `github.com/bucketvcs/bucketvcs/internal/repo`, `github.com/bucketvcs/bucketvcs/internal/storage`.

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./internal/gateway/...`
Expected: FAIL — push doesn't yet update the manifest.

- [ ] **Step 3: Write the implementation**

Replace the stub block in `handleReceivePack` (in receive_pack.go) with the full flow:

```go
	// Acquire write lock for the entire push.
	m.Lock()
	defer m.Unlock()

	// Sync mirror so old-oid checks reflect the latest authoritative state.
	if err := m.SyncToCurrent(r.Context()); err != nil {
		http.Error(w, "mirror sync: "+err.Error(), http.StatusInternalServerError)
		_ = os.Remove(req.PackPath)
		return
	}

	r2, err := repo.Open(r.Context(), s.store, tenant, repoID)
	if err != nil {
		http.Error(w, "repo: "+err.Error(), http.StatusInternalServerError)
		_ = os.Remove(req.PackPath)
		return
	}
	view, err := r2.ReadRoot(r.Context())
	if err != nil {
		http.Error(w, "manifest: "+err.Error(), http.StatusInternalServerError)
		_ = os.Remove(req.PackPath)
		return
	}
	currentBody := view.Body

	// 1. Validate old-oid for each update.
	const nullOID = "0000000000000000000000000000000000000000"
	statuses := make([]string, len(req.Updates))
	allOK := true
	for i, u := range req.Updates {
		// "create" old-oid is null
		switch {
		case u.OldOID == nullOID:
			if _, exists := currentBody.Refs[u.Refname]; exists {
				statuses[i] = "ng " + u.Refname + " ref already exists"
				allOK = false
			}
		default:
			cur, ok := currentBody.Refs[u.Refname]
			if !ok || cur != u.OldOID {
				statuses[i] = "ng " + u.Refname + " stale info"
				allOK = false
			}
		}
	}
	if req.IsAtomic && !allOK {
		for i, u := range req.Updates {
			if statuses[i] == "" {
				statuses[i] = "ng " + u.Refname + " atomic-batch-failed"
			}
		}
		writeReport(w, "unpack ok", statuses, req.Caps)
		_ = os.Remove(req.PackPath)
		return
	}

	// 2. If pack present, validate via index-pack + connectivity.
	if req.PackPath != "" {
		if _, err := gitcli.IndexPackStrict(r.Context(), m.BareDir(), req.PackPath); err != nil {
			writeReport(w, "unpack invalid-pack: "+err.Error(), nil, req.Caps)
			_ = os.Remove(req.PackPath)
			return
		}
		newOIDs := make([]string, 0, len(req.Updates))
		for _, u := range req.Updates {
			if u.NewOID != nullOID {
				newOIDs = append(newOIDs, u.NewOID)
			}
		}
		missing, err := gitcli.RevListNotAll(r.Context(), m.BareDir(), newOIDs)
		if err != nil || len(missing) > 0 {
			writeReport(w, "unpack missing-connectivity", nil, req.Caps)
			_ = os.Remove(req.PackPath)
			return
		}
	}

	// 3. Build new manifest body via importer pipeline + commit.
	updates := map[string]string{}
	refUpdates := []mirror.RefUpdate{}
	for i, u := range req.Updates {
		if statuses[i] != "" {
			continue
		}
		updates[u.Refname] = u.NewOID
		refUpdates = append(refUpdates, mirror.RefUpdate{
			Refname: u.Refname,
			OldOID:  u.OldOID,
			NewOID:  u.NewOID,
		})
	}
	newBody, err := importer.BuildAndCommit(r.Context(), s.store, tenant, repoID, req.PackPath, updates, &currentBody)
	if err != nil {
		writeReport(w, "unpack ok", labelAllNG(req.Updates, statuses, "internal-storage-error"), req.Caps)
		_ = os.Remove(req.PackPath)
		return
	}
	_ = newBody

	// 4. Bring mirror in sync with the just-committed manifest.
	view2, err := r2.ReadRoot(r.Context())
	if err != nil {
		// Rare but recoverable: the next request will resync via stale check.
		writeReport(w, "unpack ok", labelAllNG(req.Updates, statuses, "internal-sync-error"), req.Caps)
		_ = os.Remove(req.PackPath)
		return
	}
	if err := m.IngestPack(r.Context(), req.PackPath, refUpdates, view2.Version); err != nil {
		// Same: bucket succeeded but mirror update failed. Stale-rebuild on next request.
		writeReport(w, "unpack ok", labelAllNG(req.Updates, statuses, "internal-mirror-error"), req.Caps)
		return
	}

	for i, u := range req.Updates {
		if statuses[i] == "" {
			statuses[i] = "ok " + u.Refname
		}
	}
	writeReport(w, "unpack ok", statuses, req.Caps)
}

// writeReport writes either a plain pkt-line report or a side-band-wrapped
// one depending on req.Caps["side-band-64k"].
func writeReport(w io.Writer, header string, statuses []string, caps map[string]bool) {
	pw := pktline.NewWriter(w)
	if caps["side-band-64k"] {
		// Compose the report into a buffer, then ship on band 1.
		var sbBuf bytes.Buffer
		inner := pktline.NewWriter(&sbBuf)
		_ = inner.WriteString(header + "\n")
		for _, s := range statuses {
			_ = inner.WriteString(s + "\n")
		}
		_ = inner.WriteFlush()
		sb := pktline.NewSidebandWriter(pw)
		_, _ = sb.WriteData(sbBuf.Bytes())
		_ = pw.WriteFlush()
		return
	}
	_ = pw.WriteString(header + "\n")
	for _, s := range statuses {
		_ = pw.WriteString(s + "\n")
	}
	_ = pw.WriteFlush()
}

func labelAllNG(updates []updateCommand, statuses []string, reason string) []string {
	out := make([]string, len(updates))
	copy(out, statuses)
	for i, u := range updates {
		if out[i] == "" {
			out[i] = "ng " + u.Refname + " " + reason
		}
	}
	return out
}
```

Add imports for `bytes`, `github.com/bucketvcs/bucketvcs/internal/importer`, `github.com/bucketvcs/bucketvcs/internal/mirror`. Delete the previous `writeReportPlaceholder` helper.

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./internal/gateway/...`
Expected: PASS, including the end-to-end `git push` test.

- [ ] **Step 5: Commit**

```bash
git add internal/gateway/receive_pack.go internal/gateway/receive_pack_test.go
git commit -m "M3 gateway: receive-pack validate + commit + IngestPack with status reports"
```

---

## Task 18: cmd/bucketvcs serve subcommand

**Files:**
- Create: `cmd/bucketvcs/serve.go`
- Create: `cmd/bucketvcs/serve_test.go`
- Modify: `cmd/bucketvcs/main.go` (add `serve` to the subcommand router)

The subcommand wraps `gateway.NewServer(...)` plus an `http.Server` with graceful shutdown on `SIGINT`/`SIGTERM`.

Flags:
```
--addr <host:port>          (default :8080)
--store <url>               (required, e.g. "localfs:/path")
--mirror-dir <path>         (default $XDG_CACHE_HOME/bucketvcs/mirrors)
--auth-token <secret>       (also via BUCKETVCS_AUTH_TOKEN env)
--auth-scope <s>            ("write-only" or "all"; default "write-only" if --auth-token set, else "anonymous")
--max-body-bytes <n>        (default 1073741824)
--shutdown-timeout <dur>    (default 30s)
```

- [ ] **Step 1: Write the failing test**

Create `cmd/bucketvcs/serve_test.go`:

```go
package main

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestServeCommand_StartsAndStops(t *testing.T) {
	storeDir := t.TempDir()
	mirrorDir := t.TempDir()
	addr := "127.0.0.1:0" // can't use; HTTP server.Serve needs concrete port.
	// Use a fixed unlikely port for the test instead.
	addr = "127.0.0.1:38719"

	args := []string{
		"--addr", addr,
		"--store", "localfs:" + storeDir,
		"--mirror-dir", mirrorDir,
		"--shutdown-timeout", "1s",
	}
	go func() {
		var stderr bytes.Buffer
		_ = serveCommand(context.Background(), args, &stderr)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("server never came up at %s", addr)
}

func TestServeCommand_RejectsMissingStore(t *testing.T) {
	var stderr bytes.Buffer
	if err := serveCommand(context.Background(), []string{"--mirror-dir", t.TempDir()}, &stderr); err == nil {
		t.Fatalf("expected error on missing --store")
	}
	_ = filepath.Separator
	_ = os.Stat
}
```

- [ ] **Step 2: Run, confirm failure**

Run: `go test ./cmd/bucketvcs/...`
Expected: FAIL with `undefined: serveCommand`.

- [ ] **Step 3: Write the implementation**

Create `cmd/bucketvcs/serve.go`:

```go
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/gateway"
)

const defaultMirrorSubdir = "bucketvcs/mirrors"

func serveCommand(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", ":8080", "Listen address (host:port)")
	storeURL := fs.String("store", "", `Store URL (e.g. "localfs:/var/lib/bucketvcs")`)
	mirrorDir := fs.String("mirror-dir", "", "Mirror cache directory (default $XDG_CACHE_HOME/bucketvcs/mirrors)")
	authToken := fs.String("auth-token", "", "Optional bearer token (also via BUCKETVCS_AUTH_TOKEN env)")
	authScope := fs.String("auth-scope", "", `"write-only" (default if --auth-token set) or "all"`)
	maxBody := fs.Int64("max-body-bytes", 1<<30, "Per-request body limit in bytes")
	shutdownTimeout := fs.Duration("shutdown-timeout", 30*time.Second, "Graceful shutdown deadline")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *storeURL == "" {
		return errors.New("serve: --store is required")
	}
	if *mirrorDir == "" {
		*mirrorDir = defaultMirrorDir()
	}
	token := *authToken
	if token == "" {
		token = os.Getenv("BUCKETVCS_AUTH_TOKEN")
	}
	mode := gateway.AuthAnonymous
	if token != "" {
		switch strings.ToLower(*authScope) {
		case "", "write-only":
			mode = gateway.AuthWriteOnly
		case "all":
			mode = gateway.AuthAll
		default:
			return fmt.Errorf("serve: invalid --auth-scope %q", *authScope)
		}
	} else if *authScope != "" && *authScope != "anonymous" {
		return fmt.Errorf("serve: --auth-scope=%q without --auth-token", *authScope)
	}

	store, err := openStore(*storeURL)
	if err != nil {
		return fmt.Errorf("serve: open store: %w", err)
	}
	defer closeStore(store)

	srv, err := gateway.NewServer(store, gateway.Options{
		MirrorDir:    *mirrorDir,
		Version:      version(),
		AuthMode:     mode,
		AuthToken:    token,
		MaxBodyBytes: *maxBody,
	})
	if err != nil {
		return fmt.Errorf("serve: NewServer: %w", err)
	}
	defer srv.Close()

	httpSrv := &http.Server{Addr: *addr, Handler: srv}
	errCh := make(chan error, 1)
	go func() {
		errCh <- httpSrv.ListenAndServe()
	}()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	case <-sigCh:
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), *shutdownTimeout)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}

func defaultMirrorDir() string {
	if x := os.Getenv("XDG_CACHE_HOME"); x != "" {
		return filepath.Join(x, defaultMirrorSubdir)
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".cache", defaultMirrorSubdir)
	}
	return filepath.Join(os.TempDir(), "bucketvcs-mirrors")
}

// version returns the bucketvcs build version. Tied to whatever main.go
// already reports; if no version constant exists, hardcode "0.1-dev".
func version() string { return "0.1-dev" }
```

Modify `cmd/bucketvcs/main.go` — add `case "serve": return serveCommand(ctx, args, stderr)` in the subcommand router. The existing dispatcher pattern from M2 (init/inspect-manifest/import/export/cat-object) tells you the exact insertion point.

- [ ] **Step 4: Run, confirm pass**

Run: `go test ./cmd/bucketvcs/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/bucketvcs/serve.go cmd/bucketvcs/serve_test.go cmd/bucketvcs/main.go
git commit -m "M3 cmd: serve subcommand with graceful shutdown"
```

---

## Task 19: diffharness — five new fixtures

**Files:**
- Modify: `internal/diffharness/fixtures/synthetic.go` (add 5 builders)
- Modify: `internal/diffharness/fixtures/fixtures.go` (register 5 builders in the registry)
- Modify: `internal/diffharness/fixtures/fixtures_test.go` (extend coverage if registry-iterating)

The 5 new fixtures exercise gateway-specific behaviors. Unlike the M2 fixtures (which only need to round-trip via import/export), these need to model push and incremental-fetch shapes; the actual oracles consuming them land in Tasks 20–21.

Each fixture builder follows the existing pattern: `func build<Name>(t *testing.T, dir string) Fixture`. The `Fixture` struct has fields like `Name`, `Bare` (path to bare repo), and the registry list adds an entry mapping name → builder.

- [ ] **Step 1: Add the 5 builders + register**

Append to `internal/diffharness/fixtures/synthetic.go`:

```go
// buildForcePushOverwrite produces a single-branch repo whose history will
// later be force-pushed over with a divergent commit. Both versions are
// pre-built into separate refs (refs/heads/before, refs/heads/after) so the
// push-equivalence oracle can replay both shapes.
func buildForcePushOverwrite(t *testing.T, dir string) Fixture {
	work := initWork(t)
	commitFile(t, work, "a.txt", "first\n", "first")
	mustGit(t, work, "branch", "before")
	mustGit(t, work, "checkout", "-B", "after")
	mustGit(t, work, "reset", "--hard", "HEAD~0")
	commitFile(t, work, "a.txt", "DIVERGENT\n", "diverge")
	mustGit(t, work, "checkout", "before")
	buildBareFromWork(t, work, dir)
	return finalize(t, "force_push_overwrite", dir)
}

func buildDeleteBranch(t *testing.T, dir string) Fixture {
	work := initWork(t)
	commitFile(t, work, "a.txt", "main\n", "init")
	mustGit(t, work, "branch", "to-delete")
	buildBareFromWork(t, work, dir)
	return finalize(t, "delete_branch", dir)
}

func buildAtomicMultiRefPush(t *testing.T, dir string) Fixture {
	work := initWork(t)
	commitFile(t, work, "a.txt", "main\n", "init")
	mustGit(t, work, "checkout", "-B", "topic-a")
	commitFile(t, work, "a.txt", "topic-a\n", "topic-a")
	mustGit(t, work, "checkout", "main")
	mustGit(t, work, "checkout", "-B", "topic-b")
	commitFile(t, work, "b.txt", "topic-b\n", "topic-b")
	mustGit(t, work, "checkout", "main")
	buildBareFromWork(t, work, dir)
	return finalize(t, "atomic_multi_ref_push", dir)
}

func buildIncrementalFetchAfterPush(t *testing.T, dir string) Fixture {
	// Two-commit history; the oracle clones depth=1, then incrementally
	// fetches the second commit.
	work := initWork(t)
	commitFile(t, work, "a.txt", "first\n", "first")
	commitFile(t, work, "a.txt", "second\n", "second")
	buildBareFromWork(t, work, dir)
	return finalize(t, "incremental_fetch_after_push", dir)
}

func buildShallowCloneDepth1(t *testing.T, dir string) Fixture {
	// Three-commit linear history. The shallow oracle clones with depth=1
	// and asserts the local clone has only the tip.
	work := initWork(t)
	commitFile(t, work, "a.txt", "v1\n", "v1")
	commitFile(t, work, "a.txt", "v2\n", "v2")
	commitFile(t, work, "a.txt", "v3\n", "v3")
	buildBareFromWork(t, work, dir)
	return finalize(t, "shallow_clone_depth_1", dir)
}
```

Modify `internal/diffharness/fixtures/fixtures.go` — find the `Registry` slice (or whatever the existing code uses) and append:

```go
{Name: "force_push_overwrite",        Build: buildForcePushOverwrite},
{Name: "delete_branch",               Build: buildDeleteBranch},
{Name: "atomic_multi_ref_push",       Build: buildAtomicMultiRefPush},
{Name: "incremental_fetch_after_push", Build: buildIncrementalFetchAfterPush},
{Name: "shallow_clone_depth_1",       Build: buildShallowCloneDepth1},
```

If the existing registry is a function that returns the slice, modify accordingly.

- [ ] **Step 2: Run, confirm registry growth**

Run: `go test ./internal/diffharness/fixtures/...`
Expected: PASS — existing `fixtures_test.go` should iterate the registry and assert `git fsck` clean per fixture.

- [ ] **Step 3: Commit**

```bash
git add internal/diffharness/fixtures/synthetic.go internal/diffharness/fixtures/fixtures.go
git commit -m "M3 diffharness: add 5 fixtures for push/delete/atomic/incremental-fetch/shallow"
```

---

## Task 20: diffharness — clone-equivalence oracle

**Files:**
- Create: `internal/diffharness/clone_oracle_test.go`

For each fixture in the registry: import → start gateway in `httptest.NewServer` → `git clone` → recursive cat-object diff vs upstream input. Reuses M2's existing `CatObject` helper for the recursive diff.

- [ ] **Step 1: Write the test**

Create `internal/diffharness/clone_oracle_test.go`:

```go
package diffharness

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/diffharness/fixtures"
	"github.com/bucketvcs/bucketvcs/internal/gateway"
	"github.com/bucketvcs/bucketvcs/internal/importer"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"

	"net/http/httptest"
)

func TestOracle_CloneEquivalence(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	for _, fx := range fixtures.Registry {
		fx := fx
		t.Run(fx.Name, func(t *testing.T) {
			t.Parallel()
			workDir := t.TempDir()
			fixt := fx.Build(t, filepath.Join(workDir, "src"))
			storeDir := t.TempDir()
			store, err := localfs.Open(storeDir)
			if err != nil {
				t.Fatalf("localfs.Open: %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })

			tenant := "fx"
			repoID := fx.Name
			if _, err := importer.Import(context.Background(), store, tenant, repoID, fixt.Bare); err != nil {
				t.Fatalf("Import: %v", err)
			}

			srv, err := gateway.NewServer(store, gateway.Options{
				MirrorDir: t.TempDir(),
				Version:   "test",
			})
			if err != nil {
				t.Fatalf("NewServer: %v", err)
			}
			t.Cleanup(func() { _ = srv.Close() })
			ts := httptest.NewServer(srv)
			t.Cleanup(ts.Close)

			dst := filepath.Join(workDir, "clone")
			cmd := exec.Command("git", "clone", "--bare", "-c", "protocol.version=2", ts.URL+"/"+tenant+"/"+repoID+".git", dst)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("git clone: %v\n%s", err, out)
			}

			if err := DiffBareRepos(t, fixt.Bare, dst); err != nil {
				t.Fatalf("clone-equivalence diff failed: %v", err)
			}
		})
	}
}
```

`DiffBareRepos(t, srcBare, dstBare)` is the existing M2 recursive cat-object diff helper. If its name in M2 is different (e.g. `RecursiveDiff`, `CatObjectDiff`, or `compareBareRepos`), use that name verbatim. The plan author should verify by `grep` before this task.

- [ ] **Step 2: Run, confirm pass**

Run: `go test -run TestOracle_CloneEquivalence ./internal/diffharness/...`
Expected: PASS for all fixtures (16 subtests).

- [ ] **Step 3: Commit**

```bash
git add internal/diffharness/clone_oracle_test.go
git commit -m "M3 diffharness: clone-equivalence oracle (gateway clone vs upstream cat-object diff)"
```

---

## Task 21: diffharness — push-equivalence oracle

**Files:**
- Create: `internal/diffharness/push_oracle_test.go`

For each fixture: start gateway with empty bucket → `bucketvcs init` repo → `git push --mirror` from fixture's bare → `bucketvcs export` to dst → recursive cat-object diff vs fixture.

- [ ] **Step 1: Write the test**

Create `internal/diffharness/push_oracle_test.go`:

```go
package diffharness

import (
	"context"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/diffharness/fixtures"
	"github.com/bucketvcs/bucketvcs/internal/exporter"
	"github.com/bucketvcs/bucketvcs/internal/gateway"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestOracle_PushEquivalence(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	for _, fx := range fixtures.Registry {
		fx := fx
		t.Run(fx.Name, func(t *testing.T) {
			t.Parallel()
			workDir := t.TempDir()
			fixt := fx.Build(t, filepath.Join(workDir, "src"))

			storeDir := t.TempDir()
			store, err := localfs.Open(storeDir)
			if err != nil {
				t.Fatalf("localfs.Open: %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })
			tenant := "fx"
			repoID := fx.Name
			if _, err := repo.Create(context.Background(), store, tenant, repoID, repo.CreateOptions{}); err != nil {
				t.Fatalf("repo.Create: %v", err)
			}

			srv, err := gateway.NewServer(store, gateway.Options{MirrorDir: t.TempDir(), Version: "test"})
			if err != nil {
				t.Fatalf("NewServer: %v", err)
			}
			t.Cleanup(func() { _ = srv.Close() })
			ts := httptest.NewServer(srv)
			t.Cleanup(ts.Close)

			cmd := exec.Command("git", "-C", fixt.Bare, "push", "--mirror", ts.URL+"/"+tenant+"/"+repoID+".git")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("git push: %v\n%s", err, out)
			}

			dst := filepath.Join(workDir, "exported.git")
			if err := exporter.Export(context.Background(), store, tenant, repoID, dst, exporter.Options{}); err != nil {
				t.Fatalf("exporter.Export: %v", err)
			}

			if err := DiffBareRepos(t, fixt.Bare, dst); err != nil {
				t.Fatalf("push-equivalence diff failed: %v", err)
			}
		})
	}
}
```

If `exporter.Export` does not take `exporter.Options{}` in M2, drop it. Verify by `grep -n "^func Export" internal/exporter/exporter.go` first.

- [ ] **Step 2: Run, confirm pass**

Run: `go test -run TestOracle_PushEquivalence ./internal/diffharness/...`
Expected: PASS for all fixtures.

- [ ] **Step 3: Commit**

```bash
git add internal/diffharness/push_oracle_test.go
git commit -m "M3 diffharness: push-equivalence oracle (gateway push vs export cat-object diff)"
```

---

## Task 22: stress smoke test (build tag `stress`)

**Files:**
- Create: `internal/gateway/stress_test.go`

A 1000-commit synthetic repo gets fully pushed via the gateway and asserts under-60s wall time on a dev box. Mirrors M2's stress smoke posture.

- [ ] **Step 1: Write the test**

Create `internal/gateway/stress_test.go`:

```go
//go:build stress

package gateway

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestStress_Push1000Commits(t *testing.T) {
	const N = 1000

	src := filepath.Join(t.TempDir(), "src.git")
	mustExec(t, "", "git", "init", "--bare", src)
	work := filepath.Join(t.TempDir(), "wt")
	mustExec(t, "", "git", "clone", src, work)
	for i := 0; i < N; i++ {
		path := filepath.Join(work, "f.txt")
		_ = os.WriteFile(path, []byte(time.Now().String()), 0o644)
		mustExec(t, work, "git", "add", "f.txt")
		mustExec(t, work, "git", "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "c")
	}

	storeDir := t.TempDir()
	store, _ := localfs.Open(storeDir)
	t.Cleanup(func() { _ = store.Close() })
	if _, err := repo.Create(context.Background(), store, "fx", "stress", repo.CreateOptions{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	srv, _ := NewServer(store, Options{MirrorDir: t.TempDir(), Version: "test"})
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	start := time.Now()
	cmd := exec.Command("git", "-C", work, "push", ts.URL+"/fx/stress.git", "HEAD:refs/heads/main")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git push: %v\n%s", err, out)
	}
	if d := time.Since(start); d > 60*time.Second {
		t.Fatalf("stress push took %v, expected <60s", d)
	}
}
```

- [ ] **Step 2: Run, confirm pass**

Run: `go test -tags stress -timeout 120s ./internal/gateway/...`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/gateway/stress_test.go
git commit -m "M3 gateway: stress smoke (1000-commit push <60s) gated by stress tag"
```

---

## Task 23: ship gates and tag

**Files:**
- Create: `/home/eran/.claude/projects/-home-eran-work-bucketvcs/memory/m3_progress.md`
- Modify: `/home/eran/.claude/projects/-home-eran-work-bucketvcs/memory/MEMORY.md`

Final gate before merging the worktree to main and tagging `m3-complete`.

- [ ] **Step 1: Run all tests**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 2: Run vet and staticcheck**

Run:
```bash
go vet ./...
staticcheck ./...
```
Expected: no findings.

- [ ] **Step 3: Run gofmt check**

Run: `test -z "$(gofmt -l .)"`
Expected: empty.

- [ ] **Step 4: Run differential harness explicitly**

Run: `go test -run 'TestOracle_' -timeout 600s ./internal/diffharness/...`
Expected: PASS — 16 fixtures × 4 oracles = 64 oracle assertions (M2's 2 round-trip + cat-object oracles plus M3's 2 clone + push oracles).

- [ ] **Step 5: Verify known-divergences artifact still empty**

Run: `go test -run 'TestKnownDivergences' ./internal/diffharness/...`
Expected: PASS (M2's format-gate test must continue to lock the empty entries section).

- [ ] **Step 6: Run stress smoke**

Run: `go test -tags stress -timeout 120s ./internal/gateway/... ./internal/importer/...`
Expected: PASS. Not a hard gate but confirms no severe regressions.

- [ ] **Step 7: Write m3_progress.md**

Create `/home/eran/.claude/projects/-home-eran-work-bucketvcs/memory/m3_progress.md`:

```markdown
---
name: M3 git protocol gateway merged to main
description: bucketvcs M3 (HTTP smart-Git gateway) is merged into main with annotated tag m3-complete. Pure-Go pkt-line + protocol v2 (upload-pack) + protocol v0 (receive-pack), per-repo on-disk mirror kept in sync, in-process push serialization, optional shared bearer token auth.
type: project
---
M3 was started 2026-05-06 in a git worktree at `.claude/worktrees/m3-protocol-gateway` and merged into `main` at commit `<MERGE_SHA>` with annotated tag `m3-complete` on `<DATE>`.

**Why:** M3 is the first milestone exposing bucketvcs over the wire. After M3, `git clone http://localhost:PORT/<tenant>/<repo>.git` and `git push --mirror` work end-to-end against a localfs-backed repo. M3 ships the protocol layer (pkt-line, v2 dispatch, side-band, ref-advert) in pure Go, while pack assembly stays shelled out to git pack-objects/index-pack against a per-repo on-disk mirror.

**How to apply:** When the user starts M4, branch off `main` at or after the m3-complete tag. M4 (per the OSS decomposition row) builds GC and orphaned-object cleanup; it consumes the M3 mirror primitives unchanged.

**M3 public APIs — what M4+ consumes:**
- `gateway.NewServer(store, opts) *gateway.Server` — main entry point.
- `gateway.Options{MirrorDir, AuthMode, AuthToken, MaxBodyBytes, Version}` — additive in future milestones.
- `mirror.Manager` and `mirror.Mirror` — for M9 background maintenance.
- `pktline.Reader`, `pktline.Writer`, `pktline.SidebandWriter` — for SSH transport in M5.
- `v2proto.Caps`, `v2proto.HandleLsRefs`, `v2proto.ParseFetchArgs`, `v2proto.WriteAcknowledgments` — for adding capabilities later.

**Architecture invariants M4+ MUST honor:**
- HTTP URL pattern `/{tenant}/{repo}.git/...` with trailing `.git`. Don't change.
- `manifest_version.txt` sentinel format is opaque single-line string from `repo.RootView.Version`.
- Gateway never holds per-repo mutex across long bucket operations; context-cancellation must release.
- `--no-replace-objects` on every gitcli invocation (M2 invariant continues).
- `repo.Commit` is the only path advancing the manifest. Mirror is always derivable.

**Differential harness ship state:** 16 fixtures × 4 oracles = 64 assertions. `docs/superpowers/diffharness/known-divergences.md` empty entries section. Promotion rule §40.3 activated.

**Known limitations carried into M4+:**
- Partial-failure stranded objects on push (same shape as M2's importer).
- No mirror eviction / repacking — M9.
- Force pushes accepted silently — M14 protected branches.
- v2-only; v1 fallback additive if needed.
- Native TLS termination delegated to reverse proxy.
- Single shared token; no rotation — M5.
- No SSH transport — M5+.
```

- [ ] **Step 8: Update MEMORY.md**

Add line under the M2 entry in `/home/eran/.claude/projects/-home-eran-work-bucketvcs/memory/MEMORY.md`:

```markdown
- [M3 git protocol gateway merged to main](m3_progress.md) — commit <MERGE_SHA>, tag m3-complete (<DATE>); pure-Go pkt-line + v2 upload-pack + v0 receive-pack + on-disk mirror + auth
```

- [ ] **Step 9: Commit progress note**

```bash
git add /home/eran/.claude/projects/-home-eran-work-bucketvcs/memory/m3_progress.md /home/eran/.claude/projects/-home-eran-work-bucketvcs/memory/MEMORY.md
git commit -m "M3 progress note + MEMORY index update"
```

(The actual commit/tag happens at merge-back time, not in this task.)

- [ ] **Step 10: Final clean-tree verification**

Run:
```bash
git status --short
go test ./...
go vet ./...
staticcheck ./...
gofmt -l .
```
Expected: all clean. Ready to merge worktree → main and tag.

---

## Self-Review

**Spec coverage:**
- §13 HTTP routes → Tasks 12, 14, 15, 16, 17, 18.
- §13 protocol v2 + receive-pack v0 → Tasks 1–6, 14–17.
- §13 capability set → Task 3 (v2), Task 14 (v0).
- §16.1 fetch flow → Tasks 5, 6, 15.
- §17 push flow → Tasks 16, 17.
- §18 in-process per-repo serialization → Tasks 7, 8 (mutex + flock), Task 17 (Lock acquired around push).
- §41 differential harness extension → Tasks 19, 20, 21.
- M3 spec §10 URL routing tenant/repo regex → Task 12.
- M3 spec §6.2 "validate `old-oid`, index-pack --strict, fsck connectivity-only" → Task 17.
- M3 spec §7 mirror lifecycle → Tasks 7, 8, 9.
- M3 spec §9 auth A2 → Task 13.
- M3 spec §11 server binary → Task 18.
- M3 spec §12 differential oracles → Tasks 19, 20, 21.
- M3 spec §14 stress smoke → Task 22.

**Placeholder scan:** Two known soft handoffs across tasks:
- Task 9 references `gitcli.UpdateRefDelete`/`UpdateRefCAS` and stubs them in gitcli.go to keep the package compiling; Task 10 promotes them with full tests. This is intentional and called out in the task body.
- Task 11 says "shape depends on the existing M2 internal helper names; if M2 has private functions named `uploadPack`, `buildAndUploadObjectMap`, ..., they're reused as-is. If they don't exist with those names, this task is also where you give them those names by extracting them from the existing `Import` body." This is a real ambiguity that should be tightened by the implementer at Task 11 time. Acceptable for a plan because the implementer has the tree open and can grep in 30 seconds.

**Type consistency:**
- `RefUpdate` is the same shape across mirror, gateway, and importer call sites.
- `Mirror.IngestPack(ctx, packPath, []RefUpdate, newVersion)` consistent across Tasks 9, 17.
- `gateway.Options.AuthMode` / `AuthToken` consistent across Tasks 12, 13, 18.
- `manifest.Body` is the M2 wire-format struct unchanged; no new fields.
- The `view.Version` field name comes from M1's `repo.RootView`; the implementer at Task 7 should grep first to confirm the exact field name (one of `Version`, `ManifestVersion`).

---

## Execution Handoff

**Plan complete and saved to `docs/superpowers/plans/2026-05-06-m3-git-protocol-gateway.md`. Two execution options:**

**1. Subagent-Driven (recommended)** — I dispatch a fresh subagent per task, review between tasks, fast iteration, M1+ review protocol per task (superpowers code-reviewer + roborev-refine on max reasoning until pass or diminishing returns).

**2. Inline Execution** — Execute tasks in this session using executing-plans, batch execution with checkpoints for review.

**Which approach?**
