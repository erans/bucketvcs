package v2proto

import (
	"bytes"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/pktline"
)

func TestWriteV2Advertisement_ContainsExpectedLines(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteV2Advertisement(&buf, "git-upload-pack", "0.1", CapsOptions{}); err != nil {
		t.Fatalf("WriteV2Advertisement: %v", err)
	}
	tokens := drainTokens(t, &buf)

	wantLines := []string{
		"# service=git-upload-pack\n",
		"", // flush
		"version 2\n",
		"agent=bucketvcs/0.1\n",
		"ls-refs=unborn\n",
		"fetch\n",
		"object-format=sha1\n",
		"", // flush
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
	if err := WriteV2Advertisement(&buf, "git-upload-pack", "0.1\nls-refs=evil", CapsOptions{}); err == nil {
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

func TestWriteV2Advertisement_RejectsServiceWithSpace(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteV2Advertisement(&buf, "git upload-pack", "0.1", CapsOptions{}); err == nil {
		t.Fatalf("WriteV2Advertisement: expected error on service with space")
	}
	if buf.Len() != 0 {
		t.Fatalf("partial bytes written on rejection: %d", buf.Len())
	}
}

func TestWriteV2Advertisement_RejectsServiceWithControlChars(t *testing.T) {
	var buf bytes.Buffer
	for _, bad := range []string{"git\nupload-pack", "git\rupload-pack", "git\x00upload-pack"} {
		buf.Reset()
		if err := WriteV2Advertisement(&buf, bad, "0.1", CapsOptions{}); err == nil {
			t.Fatalf("WriteV2Advertisement: expected error on service %q", bad)
		}
		if buf.Len() != 0 {
			t.Fatalf("partial bytes written on rejection of %q: %d", bad, buf.Len())
		}
	}
}

// TestV2Capabilities_BundleURIConditional verifies the slice form of the cap
// list: bundle-uri absent by default, present when opted in.
func TestV2Capabilities_BundleURIConditional(t *testing.T) {
	capsOff := V2CapabilitiesWithOptions("0.1", CapsOptions{BundleURI: false})
	for _, c := range capsOff {
		if c == "bundle-uri" {
			t.Errorf("V2CapabilitiesWithOptions(BundleURI=false) should not include bundle-uri, got %v", capsOff)
			break
		}
	}
	// Ensure the standard caps are still present.
	for _, want := range []string{"version 2", "fetch", "ls-refs=unborn", "object-format=sha1"} {
		found := false
		for _, c := range capsOff {
			if c == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("V2CapabilitiesWithOptions(BundleURI=false) missing expected cap %q", want)
		}
	}

	capsOn := V2CapabilitiesWithOptions("0.1", CapsOptions{BundleURI: true})
	found := false
	for _, c := range capsOn {
		if c == "bundle-uri" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("V2CapabilitiesWithOptions(BundleURI=true) should include bundle-uri, got %v", capsOn)
	}
}

// collectCapLines parses a v2 advertisement buffer and returns the capability
// lines (the second block of data tokens, after the first flush-pkt).
func collectCapLines(t *testing.T, buf *bytes.Buffer) []string {
	t.Helper()
	tokens := drainTokens(t, buf)
	// Skip past the first flush-pkt (and any leading data lines before it).
	past := false
	var caps []string
	for _, tok := range tokens {
		if tok.Type == pktline.Flush {
			if !past {
				past = true
				continue
			}
			break
		}
		if past && tok.Type == pktline.Data {
			caps = append(caps, strings.TrimSuffix(string(tok.Payload), "\n"))
		}
	}
	return caps
}

// TestWriteV2Advertisement_BundleURICapability checks the HTTP advertisement.
func TestWriteV2Advertisement_BundleURICapability(t *testing.T) {
	for _, tc := range []struct {
		name    string
		opts    CapsOptions
		wantCap bool
	}{
		{"disabled", CapsOptions{BundleURI: false}, false},
		{"enabled", CapsOptions{BundleURI: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteV2Advertisement(&buf, "git-upload-pack", "0.1", tc.opts); err != nil {
				t.Fatalf("WriteV2Advertisement: %v", err)
			}
			caps := collectCapLines(t, &buf)
			found := false
			for _, c := range caps {
				if c == "bundle-uri" {
					found = true
					break
				}
			}
			if found != tc.wantCap {
				t.Errorf("bundle-uri found=%v, want %v; caps=%v", found, tc.wantCap, caps)
			}
		})
	}
}

// TestWriteV2AdvertisementSSH_BundleURICapability checks the SSH advertisement.
func TestWriteV2AdvertisementSSH_BundleURICapability(t *testing.T) {
	for _, tc := range []struct {
		name    string
		opts    CapsOptions
		wantCap bool
	}{
		{"disabled", CapsOptions{BundleURI: false}, false},
		{"enabled", CapsOptions{BundleURI: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteV2AdvertisementSSH(&buf, "0.1", tc.opts); err != nil {
				t.Fatalf("WriteV2AdvertisementSSH: %v", err)
			}
			// SSH advertisement has no preamble — all data tokens are caps.
			tokens := drainTokens(t, &buf)
			var caps []string
			for _, tok := range tokens {
				if tok.Type == pktline.Data {
					caps = append(caps, strings.TrimSuffix(string(tok.Payload), "\n"))
				}
			}
			found := false
			for _, c := range caps {
				if c == "bundle-uri" {
					found = true
					break
				}
			}
			if found != tc.wantCap {
				t.Errorf("bundle-uri found=%v, want %v; caps=%v", found, tc.wantCap, caps)
			}
		})
	}
}
