package uploadpack

import (
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/pktline"
)

// TestFindCommandLine covers the four cases that the protocol-v2 dispatcher
// scan must handle. The original logic lived inline in serviceImpl; it was
// hoisted into findCommandLine in M11 phase 10 so the scan is testable
// independently of repo.Open / fixture setup.
//
// Cases:
//
//	(a) command= at position 0 (pre-bundle-uri layout; git <2.54 fetch path).
//	(b) Capability lines first ("agent=", "object-format="), then "command="
//	    (the layout git 2.54's bundle-uri client emits; LOW-2 motivation).
//	(c) Leading non-Data token (Delim/Flush at position 0): must report
//	    "no command found" so the caller emits ErrBadRequest.
//	(d) No "command=" line anywhere in the leading Data run: same outcome.
//
// We assert directly on the (cmdLine, ok) return tuple so the test is
// independent of repo state, fixture builders, and ErrBadRequest's exact
// wrapping — those are exercised end-to-end by bundleuri_test.go.

func dataTok(s string) pktline.Token {
	return pktline.Token{Type: pktline.Data, Payload: []byte(s)}
}

// (a) command= at position 0 — the historical layout that
// findCommandLine must continue to accept unchanged.
func TestFindCommandLine_CommandAtPositionZero(t *testing.T) {
	tokens := []pktline.Token{
		dataTok("command=ls-refs\n"),
		dataTok("agent=git/2.40.0\n"),
		{Type: pktline.Delim},
		dataTok("ref-prefix refs/heads/\n"),
		{Type: pktline.Flush},
	}
	got, ok := findCommandLine(tokens)
	if !ok {
		t.Fatalf("findCommandLine returned ok=false; want command=ls-refs")
	}
	if got != "command=ls-refs" {
		t.Fatalf("findCommandLine = %q, want %q", got, "command=ls-refs")
	}
}

// (b) Capabilities before command=, mirroring git 2.54 bundle-uri client.
// This is the case the refactor was introduced to support.
func TestFindCommandLine_CapabilitiesBeforeCommand(t *testing.T) {
	tokens := []pktline.Token{
		dataTok("agent=git/2.54.0\n"),
		dataTok("object-format=sha1\n"),
		dataTok("command=bundle-uri\n"),
		{Type: pktline.Flush},
	}
	got, ok := findCommandLine(tokens)
	if !ok {
		t.Fatalf("findCommandLine returned ok=false; want command=bundle-uri")
	}
	if got != "command=bundle-uri" {
		t.Fatalf("findCommandLine = %q, want %q", got, "command=bundle-uri")
	}
}

// (c) Leading non-Data token — the dispatcher's "stray Delim/Flush at the
// front" case. The scan must stop immediately and report no command.
func TestFindCommandLine_LeadingNonDataToken(t *testing.T) {
	tokens := []pktline.Token{
		{Type: pktline.Delim},
		dataTok("command=ls-refs\n"),
		{Type: pktline.Flush},
	}
	got, ok := findCommandLine(tokens)
	if ok {
		t.Fatalf("findCommandLine returned ok=true (%q); want ok=false for leading non-Data", got)
	}
	if got != "" {
		t.Fatalf("findCommandLine = %q, want empty string on no-command path", got)
	}
}

// (d) No command= anywhere — only capability lines. The scan exhausts
// the slice without a match and reports no command.
func TestFindCommandLine_NoCommandLine(t *testing.T) {
	tokens := []pktline.Token{
		dataTok("agent=git/2.54.0\n"),
		dataTok("object-format=sha1\n"),
		{Type: pktline.Flush},
	}
	got, ok := findCommandLine(tokens)
	if ok {
		t.Fatalf("findCommandLine returned ok=true (%q); want ok=false when no command= present", got)
	}
	if got != "" {
		t.Fatalf("findCommandLine = %q, want empty string on no-command path", got)
	}
}
