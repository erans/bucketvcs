package v2proto

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// validPackChecksum is a 40-hex lowercase string used in the happy-path
// gate evaluation. Picked to be visually distinguishable from common
// fixture values so test failures point at the right place.
const validPackChecksum = "0123456789abcdef0123456789abcdef01234567"

// fakeBuildURL returns a closure that records the args it was called
// with and returns the configured (url, err) pair. Use to assert the
// gate threads PackChecksum/PackKey through correctly.
func fakeBuildURL(url string, err error) (func(context.Context, string, string, string) (string, error), *struct {
	called             bool
	hash, key, expHash string
}) {
	rec := &struct {
		called             bool
		hash, key, expHash string
	}{}
	return func(_ context.Context, hash, key, expectedHash string) (string, error) {
		rec.called = true
		rec.hash = hash
		rec.key = key
		rec.expHash = expectedHash
		return url, err
	}, rec
}

// TestEvaluatePackURIAdvertise_Eligible covers the happy path: all gates
// pass, BuildURL succeeds, the stanza carries "<sha1> <uri>\n".
func TestEvaluatePackURIAdvertise_Eligible(t *testing.T) {
	build, rec := fakeBuildURL("https://cdn.example.com/pack.pack", nil)
	res, err := EvaluatePackURIAdvertise(context.Background(), PackURIInputs{
		ClientOptedIn:     true,
		FullPackRequested: true,
		PackChecksum:      validPackChecksum,
		PackKey:           "tenants/t/repos/r/packs/canonical/sha256-ff.pack",
		PackID:            validPackChecksum,
		BuildURL:          build,
	})
	if err != nil {
		t.Fatalf("EvaluatePackURIAdvertise: %v", err)
	}
	want := validPackChecksum + " https://cdn.example.com/pack.pack\n"
	if res.Stanza != want {
		t.Fatalf("Stanza: got %q, want %q", res.Stanza, want)
	}
	if res.URL != "https://cdn.example.com/pack.pack" {
		t.Fatalf("URL: got %q, want %q", res.URL, "https://cdn.example.com/pack.pack")
	}
	if !rec.called {
		t.Fatalf("BuildURL was not called")
	}
	if rec.hash != validPackChecksum {
		t.Errorf("BuildURL hash arg: got %q, want %q", rec.hash, validPackChecksum)
	}
	if rec.key != "tenants/t/repos/r/packs/canonical/sha256-ff.pack" {
		t.Errorf("BuildURL key arg: got %q", rec.key)
	}
}

// TestEvaluatePackURIAdvertise_NotOptedIn — client did not send
// packfile-uris=, so the gate must short-circuit before BuildURL.
func TestEvaluatePackURIAdvertise_NotOptedIn(t *testing.T) {
	build, rec := fakeBuildURL("https://cdn.example.com/pack.pack", nil)
	res, err := EvaluatePackURIAdvertise(context.Background(), PackURIInputs{
		ClientOptedIn:     false,
		FullPackRequested: true,
		PackChecksum:      validPackChecksum,
		PackKey:           "k",
		PackID:            validPackChecksum,
		BuildURL:          build,
	})
	if err != nil {
		t.Fatalf("EvaluatePackURIAdvertise: %v", err)
	}
	if res.Stanza != "" {
		t.Fatalf("Stanza: got %q, want empty", res.Stanza)
	}
	if rec.called {
		t.Fatalf("BuildURL must not be called when client did not opt in")
	}
}

// TestEvaluatePackURIAdvertise_NotFullPack — request not fully covered by
// a single canonical pack; gate must skip without invoking BuildURL.
func TestEvaluatePackURIAdvertise_NotFullPack(t *testing.T) {
	build, rec := fakeBuildURL("https://cdn.example.com/pack.pack", nil)
	res, err := EvaluatePackURIAdvertise(context.Background(), PackURIInputs{
		ClientOptedIn:     true,
		FullPackRequested: false,
		PackChecksum:      validPackChecksum,
		PackKey:           "k",
		PackID:            validPackChecksum,
		BuildURL:          build,
	})
	if err != nil {
		t.Fatalf("EvaluatePackURIAdvertise: %v", err)
	}
	if res.Stanza != "" {
		t.Fatalf("Stanza: got %q, want empty", res.Stanza)
	}
	if rec.called {
		t.Fatalf("BuildURL must not be called when !FullPackRequested")
	}
}

// TestEvaluatePackURIAdvertise_MissingChecksum_Skips covers the case where
// the canonical pack lacks a PackChecksum (legacy pre-M11 packs that
// haven't been backfilled). Gate skips silently.
func TestEvaluatePackURIAdvertise_MissingChecksum_Skips(t *testing.T) {
	build, rec := fakeBuildURL("https://cdn.example.com/pack.pack", nil)
	res, err := EvaluatePackURIAdvertise(context.Background(), PackURIInputs{
		ClientOptedIn:     true,
		FullPackRequested: true,
		PackChecksum:      "",
		PackKey:           "k",
		PackID:            validPackChecksum,
		BuildURL:          build,
	})
	if err != nil {
		t.Fatalf("EvaluatePackURIAdvertise: %v", err)
	}
	if res.Stanza != "" {
		t.Fatalf("Stanza: got %q, want empty", res.Stanza)
	}
	if rec.called {
		t.Fatalf("BuildURL must not be called when PackChecksum is empty")
	}
}

// TestEvaluatePackURIAdvertise_MissingPackKey_Skips covers a manifest
// inconsistency where PackKey is empty. Gate skips silently.
func TestEvaluatePackURIAdvertise_MissingPackKey_Skips(t *testing.T) {
	build, rec := fakeBuildURL("https://cdn.example.com/pack.pack", nil)
	res, err := EvaluatePackURIAdvertise(context.Background(), PackURIInputs{
		ClientOptedIn:     true,
		FullPackRequested: true,
		PackChecksum:      validPackChecksum,
		PackKey:           "",
		BuildURL:          build,
	})
	if err != nil {
		t.Fatalf("EvaluatePackURIAdvertise: %v", err)
	}
	if res.Stanza != "" {
		t.Fatalf("Stanza: got %q, want empty", res.Stanza)
	}
	if rec.called {
		t.Fatalf("BuildURL must not be called when PackKey is empty")
	}
}

// TestEvaluatePackURIAdvertise_MissingPackID_Skips covers a manifest
// inconsistency where PackID is empty. Gate skips silently — without
// PackID we cannot derive the bare-mirror keep-pack basename, so the
// inline-pack elision would fail mid-response if we advertised.
func TestEvaluatePackURIAdvertise_MissingPackID_Skips(t *testing.T) {
	build, rec := fakeBuildURL("https://cdn.example.com/pack.pack", nil)
	res, err := EvaluatePackURIAdvertise(context.Background(), PackURIInputs{
		ClientOptedIn:     true,
		FullPackRequested: true,
		PackChecksum:      validPackChecksum,
		PackKey:           "k",
		PackID:            "",
		BuildURL:          build,
	})
	if err != nil {
		t.Fatalf("EvaluatePackURIAdvertise: %v", err)
	}
	if res.Stanza != "" {
		t.Fatalf("Stanza: got %q, want empty", res.Stanza)
	}
	if rec.called {
		t.Fatalf("BuildURL must not be called when PackID is empty")
	}
}

// TestEvaluatePackURIAdvertise_InvalidPackIDFormat — PackID must be 40
// lowercase hex, same shape as PackChecksum and as gitcli.validPackBasename
// requires for `pack-<id>.pack`. Validating at the advertise gate keeps
// stanza emission and downstream keep-pack elision in lockstep: skip the
// whole URI advertise rather than emit a stanza paired with a basename
// that the inline-pack stage would reject mid-response.
func TestEvaluatePackURIAdvertise_InvalidPackIDFormat(t *testing.T) {
	for name, id := range map[string]string{
		"uppercase": strings.ToUpper(validPackChecksum),
		"too_short": validPackChecksum[:39],
		"too_long":  validPackChecksum + "a",
		"non_hex":   "0123456789abcdef0123456789abcdef0123456g",
	} {
		t.Run(name, func(t *testing.T) {
			build, rec := fakeBuildURL("https://cdn.example.com/pack.pack", nil)
			res, err := EvaluatePackURIAdvertise(context.Background(), PackURIInputs{
				ClientOptedIn:     true,
				FullPackRequested: true,
				PackChecksum:      validPackChecksum,
				PackKey:           "k",
				PackID:            id,
				BuildURL:          build,
			})
			if err != nil {
				t.Fatalf("EvaluatePackURIAdvertise: %v", err)
			}
			if res.Stanza != "" {
				t.Fatalf("Stanza: got %q, want empty (bad PackID %q)", res.Stanza, id)
			}
			if rec.called {
				t.Fatalf("BuildURL must not be called for malformed PackID")
			}
		})
	}
}

// TestEvaluatePackURIAdvertise_BuildURLError_Skips — BuildURL returned an
// error; gate must produce empty stanza with nil error so the caller
// falls through to the inline pack section without disturbing the
// response stream.
func TestEvaluatePackURIAdvertise_BuildURLError_Skips(t *testing.T) {
	build, _ := fakeBuildURL("", errors.New("backend unreachable"))
	res, err := EvaluatePackURIAdvertise(context.Background(), PackURIInputs{
		ClientOptedIn:     true,
		FullPackRequested: true,
		PackChecksum:      validPackChecksum,
		PackKey:           "k",
		PackID:            validPackChecksum,
		BuildURL:          build,
	})
	if err != nil {
		t.Fatalf("EvaluatePackURIAdvertise: got err %v, want nil (soft skip)", err)
	}
	if res.Stanza != "" {
		t.Fatalf("Stanza: got %q, want empty", res.Stanza)
	}
}

// TestEvaluatePackURIAdvertise_BuildURLEmptyString_Skips — BuildURL
// returned ("", nil) which signals a misconfigured backend. Gate treats
// it identically to an error.
func TestEvaluatePackURIAdvertise_BuildURLEmptyString_Skips(t *testing.T) {
	build, _ := fakeBuildURL("", nil)
	res, err := EvaluatePackURIAdvertise(context.Background(), PackURIInputs{
		ClientOptedIn:     true,
		FullPackRequested: true,
		PackChecksum:      validPackChecksum,
		PackKey:           "k",
		PackID:            validPackChecksum,
		BuildURL:          build,
	})
	if err != nil {
		t.Fatalf("EvaluatePackURIAdvertise: got err %v, want nil", err)
	}
	if res.Stanza != "" {
		t.Fatalf("Stanza: got %q, want empty", res.Stanza)
	}
}

// TestEvaluatePackURIAdvertise_RejectsControlCharsInURI — the URI must
// not contain CR/LF/NUL or it would corrupt the pkt-line frame.
// Table-driven across each forbidden character.
func TestEvaluatePackURIAdvertise_RejectsControlCharsInURI(t *testing.T) {
	for name, ch := range map[string]string{
		"CR":  "\r",
		"LF":  "\n",
		"NUL": "\x00",
	} {
		t.Run(name, func(t *testing.T) {
			build, _ := fakeBuildURL("https://cdn.example.com/pack"+ch+"injected", nil)
			res, err := EvaluatePackURIAdvertise(context.Background(), PackURIInputs{
				ClientOptedIn:     true,
				FullPackRequested: true,
				PackChecksum:      validPackChecksum,
				PackKey:           "k",
				PackID:            validPackChecksum,
				BuildURL:          build,
			})
			if err != nil {
				t.Fatalf("EvaluatePackURIAdvertise: %v", err)
			}
			if res.Stanza != "" {
				t.Fatalf("Stanza: got %q, want empty (URI contained %s)", res.Stanza, name)
			}
		})
	}
}

// TestEvaluatePackURIAdvertise_InvalidChecksumFormat — non-canonical
// checksums (uppercase, wrong length, non-hex) are treated as upstream
// bugs: skip silently rather than emit a malformed stanza.
func TestEvaluatePackURIAdvertise_InvalidChecksumFormat(t *testing.T) {
	for name, sum := range map[string]string{
		"uppercase": strings.ToUpper(validPackChecksum),
		"too_short": validPackChecksum[:39],
		"too_long":  validPackChecksum + "a",
		"non_hex":   "0123456789abcdef0123456789abcdef0123456g",
	} {
		t.Run(name, func(t *testing.T) {
			build, rec := fakeBuildURL("https://cdn.example.com/pack.pack", nil)
			res, err := EvaluatePackURIAdvertise(context.Background(), PackURIInputs{
				ClientOptedIn:     true,
				FullPackRequested: true,
				PackChecksum:      sum,
				PackKey:           "k",
				PackID:            validPackChecksum,
				BuildURL:          build,
			})
			if err != nil {
				t.Fatalf("EvaluatePackURIAdvertise: %v", err)
			}
			if res.Stanza != "" {
				t.Fatalf("Stanza: got %q, want empty (bad checksum %q)", res.Stanza, sum)
			}
			if rec.called {
				t.Fatalf("BuildURL must not be called for malformed checksum")
			}
		})
	}
}

// TestEvaluatePackURIAdvertise_NilBuildURL — when BuildURL is nil the
// gate must skip without panicking.
func TestEvaluatePackURIAdvertise_NilBuildURL(t *testing.T) {
	res, err := EvaluatePackURIAdvertise(context.Background(), PackURIInputs{
		ClientOptedIn:     true,
		FullPackRequested: true,
		PackChecksum:      validPackChecksum,
		PackKey:           "k",
		PackID:            validPackChecksum,
		BuildURL:          nil,
	})
	if err != nil {
		t.Fatalf("EvaluatePackURIAdvertise: %v", err)
	}
	if res.Stanza != "" {
		t.Fatalf("Stanza: got %q, want empty (nil BuildURL)", res.Stanza)
	}
}
