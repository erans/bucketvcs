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
//	pkt-line: "fetch\n"
//	pkt-line: "object-format=sha1\n"
//	flush
//
// Note: "fetch" is advertised without the "=shallow" feature qualifier
// because the gateway does not (yet) implement shallow-info plumbing.
// Compliant clients MUST NOT send shallow/deepen arguments unless the
// server advertises that feature, so this prevents protocol-level failures
// for shallow clones. The fetch handler still defensively rejects shallow
// arguments in case a non-compliant client sends them.
func WriteV2Advertisement(w io.Writer, service, version string) error {
	if strings.ContainsAny(version, "\r\n\x00") {
		return fmt.Errorf("v2proto: agent version contains forbidden control characters")
	}
	if strings.ContainsAny(service, "\r\n\x00 ") {
		return fmt.Errorf("v2proto: service contains forbidden character (CR/LF/NUL/space)")
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

// WriteV2AdvertisementSSH writes the SSH-transport capability advertisement
// for protocol v2. Unlike WriteV2Advertisement, it does NOT emit the
// "# service=...\n" Smart-HTTP preamble — git SSH clients expect the
// advertisement to begin directly with "version 2\n".
//
// Layout:
//
//	pkt-line: "version 2\n"
//	pkt-line: "agent=bucketvcs/<version>\n"
//	pkt-line: "ls-refs=unborn\n"
//	pkt-line: "fetch\n"
//	pkt-line: "object-format=sha1\n"
//	flush
func WriteV2AdvertisementSSH(w io.Writer, version string) error {
	if strings.ContainsAny(version, "\r\n\x00") {
		return fmt.Errorf("v2proto: agent version contains forbidden control characters")
	}
	pw := pktline.NewWriter(w)
	for _, line := range V2Capabilities(version) {
		if err := pw.WriteString(line + "\n"); err != nil {
			return err
		}
	}
	return pw.WriteFlush()
}
func V2Capabilities(version string) []string {
	return []string{
		"version 2",
		"agent=" + AgentName + "/" + version,
		"ls-refs=unborn",
		"fetch",
		"object-format=sha1",
	}
}
