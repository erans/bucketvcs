package v2proto

import (
	"bytes"
	"strings"
	"testing"
)

func TestEncodeBundleURIResponse_OneBundle(t *testing.T) {
	var buf bytes.Buffer
	err := EncodeBundleURIResponse(&buf, []BundleAdvertisement{{
		ID:          "bundle_t_r_42_aa",
		URI:         "https://example/u",
		CreationTok: "1715346000",
	}})
	if err != nil {
		t.Fatalf("EncodeBundleURIResponse: %v", err)
	}
	s := buf.String()
	for _, want := range []string{
		"bundle.version=1",
		"bundle.mode=all",
		"bundle.bundle_t_r_42_aa.uri=https://example/u",
		"bundle.bundle_t_r_42_aa.creationToken=1715346000",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("response missing %q\nfull:\n%s", want, s)
		}
	}
	// bundle.version/mode must precede the per-bundle keys: clients that
	// stream-parse without buffering will reject the list if mode arrives
	// late. Anchor the order with a substring check.
	if idx := strings.Index(s, "bundle.mode=all"); idx < 0 || idx > strings.Index(s, "bundle.bundle_t_r_42_aa.uri") {
		t.Errorf("bundle.mode must precede per-bundle keys; got order:\n%s", s)
	}
}

func TestEncodeBundleURIResponse_Empty_NoBundles(t *testing.T) {
	var buf bytes.Buffer
	err := EncodeBundleURIResponse(&buf, nil)
	if err != nil {
		t.Fatalf("EncodeBundleURIResponse: %v", err)
	}
	// An empty response is well-formed (just the flush-pkt) — clients
	// fall through to standard fetch. The header keys are omitted because
	// they only make sense when at least one bundle follows.
	if buf.Len() == 0 {
		t.Errorf("expected at least a flush-pkt, got empty response")
	}
	if strings.Contains(buf.String(), "bundle.version") || strings.Contains(buf.String(), "bundle.mode") {
		t.Errorf("empty response must not include header keys; got:\n%s", buf.String())
	}
}

func TestEncodeBundleURIResponse_RejectsControlChars(t *testing.T) {
	cases := []struct {
		name string
		ad   BundleAdvertisement
	}{
		{name: "ID with LF", ad: BundleAdvertisement{ID: "bad\nid", URI: "https://example/u"}},
		{name: "ID with CR", ad: BundleAdvertisement{ID: "bad\rid", URI: "https://example/u"}},
		{name: "ID with NUL", ad: BundleAdvertisement{ID: "bad\x00id", URI: "https://example/u"}},
		{name: "URI with LF", ad: BundleAdvertisement{ID: "ok", URI: "https://example/\nu"}},
		{name: "URI with CR", ad: BundleAdvertisement{ID: "ok", URI: "https://example/\ru"}},
		{name: "URI with NUL", ad: BundleAdvertisement{ID: "ok", URI: "https://example/\x00u"}},
		{name: "CreationTok with LF", ad: BundleAdvertisement{ID: "ok", URI: "https://example/u", CreationTok: "12\n34"}},
		{name: "CreationTok with CR", ad: BundleAdvertisement{ID: "ok", URI: "https://example/u", CreationTok: "12\r34"}},
		{name: "CreationTok with NUL", ad: BundleAdvertisement{ID: "ok", URI: "https://example/u", CreationTok: "12\x0034"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := EncodeBundleURIResponse(&buf, []BundleAdvertisement{tc.ad})
			if err == nil {
				t.Fatalf("expected error for %s, got nil; output:\n%s", tc.name, buf.String())
			}
		})
	}
}
