// Package v2proto implements Git protocol v2 server-side dispatch:
// capability advertisement, ls-refs, and fetch.
//
// References:
//
//	https://git-scm.com/docs/protocol-v2
//	https://git-scm.com/docs/protocol-capabilities
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
//	pkt-line: "# service=<service>\n"
//	flush
//	pkt-line: "version 2\n"
//	pkt-line: "agent=bucketvcs/<version>\n"
//	pkt-line: "ls-refs=unborn\n"
//	pkt-line: "fetch=shallow\n"
//	pkt-line: "object-format=sha1\n"
//	flush
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
