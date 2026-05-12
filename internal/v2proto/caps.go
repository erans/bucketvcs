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

// CapsOptions controls which optional capabilities are included in the v2
// advertisement. The zero value advertises no optional capabilities.
type CapsOptions struct {
	// BundleURI, when true, includes "bundle-uri" in the capability list.
	// Sourced from EngineRequest.BundleURIEnabled, which the gateway and
	// sshd transports populate from their respective Options.
	BundleURI bool
	// PackURI, when true, appends "packfile-uris" as a feature qualifier
	// on the "fetch" command capability line (i.e. emits
	// "fetch=packfile-uris" instead of bare "fetch"). Per Git protocol-v2,
	// packfile-uris is a sub-feature of the fetch command, not a top-
	// level cap; the client's fetch-pack uses server_supports_feature
	// ("fetch", "packfile-uris", 0) to detect support. The supported-
	// protocol list (https only on this server) is communicated by the
	// client in its `packfile-uris <protos>` request line, not in the
	// advertisement. Sourced from EngineRequest.PackURIEnabled.
	PackURI bool
}

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
//	pkt-line: "fetch\n"               // or "fetch=packfile-uris\n" when opts.PackURI
//	pkt-line: "object-format=sha1\n"
//	[pkt-line: "bundle-uri\n"]   // only when opts.BundleURI is true
//	flush
//
// Note: "fetch" is advertised without the "=shallow" feature qualifier
// because the gateway does not (yet) implement shallow-info plumbing.
// Compliant clients MUST NOT send shallow/deepen arguments unless the
// server advertises that feature, so this prevents protocol-level failures
// for shallow clones. The fetch handler still defensively rejects shallow
// arguments in case a non-compliant client sends them.
func WriteV2Advertisement(w io.Writer, service, version string, opts CapsOptions) error {
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
	for _, line := range V2CapabilitiesWithOptions(version, opts) {
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
//	pkt-line: "fetch\n"               // or "fetch=packfile-uris\n" when opts.PackURI
//	pkt-line: "object-format=sha1\n"
//	[pkt-line: "bundle-uri\n"]   // only when opts.BundleURI is true
//	flush
func WriteV2AdvertisementSSH(w io.Writer, version string, opts CapsOptions) error {
	if strings.ContainsAny(version, "\r\n\x00") {
		return fmt.Errorf("v2proto: agent version contains forbidden control characters")
	}
	pw := pktline.NewWriter(w)
	for _, line := range V2CapabilitiesWithOptions(version, opts) {
		if err := pw.WriteString(line + "\n"); err != nil {
			return err
		}
	}
	return pw.WriteFlush()
}

// V2Capabilities returns the list of capability advertisement lines for
// protocol v2 with no optional capabilities enabled. It is preserved as a
// backwards-compatible wrapper around V2CapabilitiesWithOptions.
func V2Capabilities(version string) []string {
	return V2CapabilitiesWithOptions(version, CapsOptions{})
}

// V2CapabilitiesWithOptions returns the capability advertisement lines for
// protocol v2, conditionally including optional capabilities per opts.
// Each string is a bare capability name (no trailing newline).
//
// packfile-uris is advertised per Git protocol-v2 §Capability Advertisement
// as a feature on the "fetch" command capability, not as a separate top-
// level cap. The client's `fetch-pack.c` uses
// `server_supports_feature("fetch", "packfile-uris", 0)` to detect server
// support; that helper checks for the bare feature name in the space-
// separated value list on the "fetch" line. A standalone
// "packfile-uris=https\n" line — which we previously emitted — does NOT
// trigger that detection, so the client silently skips sending its
// `packfile-uris <protos>` request line and the in-fetch advertise gate
// never fires. See git/upload-pack.c:upload_pack_advertise where
// `strbuf_addstr(value, " packfile-uris")` is appended onto the fetch
// capability's value buffer for the canonical wire shape.
//
// The `=https` suffix the old code carried was a server-side hint about
// which schemes the server would mint (HTTPS only). Per the spec, the
// supported-protocol list is communicated by the CLIENT in its
// `packfile-uris <protos>` request line and the server simply chooses
// whichever URLs honor that list. So the suffix is dropped here; the
// server's HTTPS-only behavior is enforced when minting URLs, not in
// the cap advertisement.
func V2CapabilitiesWithOptions(version string, opts CapsOptions) []string {
	fetchCap := "fetch"
	if opts.PackURI {
		// Single sub-feature on the "fetch" command cap. If a second
		// sub-feature is added later, switch to space-separated
		// composition (e.g. "fetch=packfile-uris shallow") at that time.
		fetchCap = "fetch=packfile-uris"
	}
	caps := []string{
		"version 2",
		"agent=" + AgentName + "/" + version,
		"ls-refs=unborn",
		fetchCap,
		"object-format=sha1",
	}
	if opts.BundleURI {
		caps = append(caps, "bundle-uri")
	}
	return caps
}
