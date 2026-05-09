package receivepack

import (
	"bytes"
	"io"

	"github.com/bucketvcs/bucketvcs/internal/pktline"
)

// writeReceiveReport emits a report-status pkt-line stream per
// pack-protocol(5):
//
//	"unpack <header>\n"   (header == "ok" on success, else an error
//	                       string; clients display this verbatim)
//	per-ref status line   (each entry is either "ok <ref>\n" or
//	                       "ng <ref> <reason>\n"; pre-built by the
//	                       caller in the statuses slice)
//	flush
//
// When the client negotiated side-band-64k the entire pkt-line stream is
// multiplexed on band 1, terminated by a band-level flush, then a final
// outer pkt-line flush. We can't side-band each pkt-line individually
// because the client's report-status parser expects a contiguous pkt-line
// stream on band 1.
//
// Best-effort: if a Write fails we cannot recover (the response is
// partially written and surfacing an error would corrupt framing
// further). Errors are silently dropped; the client will see an EOF and
// surface the partial report.
func writeReceiveReport(w io.Writer, header string, statuses []string, caps map[string]bool) {
	pw := pktline.NewWriter(w)

	if caps["side-band-64k"] {
		var inner bytes.Buffer
		ipw := pktline.NewWriter(&inner)
		if err := ipw.WriteString("unpack " + header + "\n"); err != nil {
			return
		}
		for _, s := range statuses {
			if s == "" {
				continue
			}
			if err := ipw.WriteString(s + "\n"); err != nil {
				return
			}
		}
		if err := ipw.WriteFlush(); err != nil {
			return
		}
		sb := pktline.NewSidebandWriter(pw)
		if _, err := sb.WriteData(inner.Bytes()); err != nil {
			return
		}
		_ = pw.WriteFlush()
		return
	}

	if err := pw.WriteString("unpack " + header + "\n"); err != nil {
		return
	}
	for _, s := range statuses {
		if s == "" {
			continue
		}
		if err := pw.WriteString(s + "\n"); err != nil {
			return
		}
	}
	_ = pw.WriteFlush()
}
