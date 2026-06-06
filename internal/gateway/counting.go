package gateway

import (
	"io"
	"net/http"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// usageActor derives the usage-stream actor string from an authenticated
// actor: prefer the human-readable Name, fall back to the UserID, and use
// "anonymous" when there is no actor (public-read flows have a nil actor).
func usageActor(a *auth.Actor) string {
	if a == nil {
		return "anonymous"
	}
	if a.Name != "" {
		return a.Name
	}
	if a.UserID != "" {
		return a.UserID
	}
	return "anonymous"
}

// countingResponseWriter wraps an http.ResponseWriter and records the number
// of body bytes written through it. It is the single shared counting writer
// for the gateway package: the proxied bundle/pack handler uses it to report
// bytes_served, and the smart-HTTP handlers (upload-pack) use it to meter
// fetch response bytes for the usage stream.
//
// Flush is promoted from the wrapped writer because git smart-HTTP responses
// are streamed: net/http's chunked-transfer flushing is invoked by the
// protocol layer via http.Flusher, and dropping it would buffer the whole
// response. http.Hijacker/io.ReaderFrom are intentionally NOT promoted — the
// gateway smart-HTTP and proxied serve paths only do plain Write (no
// connection takeover; the engine copies via io.Copy into this writer, which
// does not assert ReaderFrom on the destination once it is wrapped).
type countingResponseWriter struct {
	http.ResponseWriter
	n int64
}

func (w *countingResponseWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.n += int64(n)
	return n, err
}

// Flush passes through to the wrapped writer when it implements http.Flusher.
// Required for git smart-HTTP streaming (chunked pkt-line flushes).
func (w *countingResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// countingReader wraps an io.Reader and counts bytes read through it. Used by
// the receive-pack handler to meter the uploaded packfile bytes (the push
// payload arrives on the request body, not the response writer).
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
