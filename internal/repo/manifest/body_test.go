package manifest

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite golden files from current Body marshal output")

func TestBody_GoldenMinimal(t *testing.T) {
	body := Body{
		DefaultBranch: "refs/heads/main",
		Refs:          map[string]string{},
		Packs:         []PackEntry{},
		Indexes:       Indexes{},
	}
	checkGolden(t, "m2-body-minimal.json", body)
}

func TestBody_GoldenSinglePack(t *testing.T) {
	body := Body{
		DefaultBranch: "refs/heads/main",
		Refs: map[string]string{
			"refs/heads/main": "0123456789abcdef0123456789abcdef01234567",
			"refs/tags/v1":    "1123456789abcdef0123456789abcdef01234567",
		},
		Packs: []PackEntry{
			{
				PackID:      "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				PackKey:     "tenants/t/repos/r/packs/canonical/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.pack",
				IdxKey:      "tenants/t/repos/r/packs/canonical/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.idx",
				SizeBytes:   4096,
				ObjectCount: 7,
			},
		},
		Indexes: Indexes{
			ObjectMap: &IndexRef{
				Key:  "tenants/t/repos/r/indexes/object-map/cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc.bvom",
				Hash: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
			},
			CommitGraph: &IndexRef{
				Key:  "tenants/t/repos/r/indexes/commit-graphs/dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd.graph",
				Hash: "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
			},
		},
	}
	checkGolden(t, "m2-body-single-pack.json", body)
}

func TestBody_RoundTrip(t *testing.T) {
	cases := []string{"m2-body-minimal.json", "m2-body-single-pack.json"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata/golden", name))
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			var b Body
			if err := json.Unmarshal(data, &b); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got, err := MarshalBody(b)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !bytes.Equal(canonicalize(t, data), canonicalize(t, got)) {
				t.Fatalf("round-trip mismatch.\nwant:\n%s\ngot:\n%s",
					canonicalize(t, data), canonicalize(t, got))
			}
		})
	}
}

func checkGolden(t *testing.T, name string, body Body) {
	t.Helper()
	got, err := MarshalBody(body)
	if err != nil {
		t.Fatalf("MarshalBody: %v", err)
	}
	path := filepath.Join("testdata/golden", name)
	if *updateGolden {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	wantC := canonicalize(t, want)
	gotC := canonicalize(t, got)
	if !bytes.Equal(wantC, gotC) {
		t.Fatalf("golden mismatch %s.\nwant:\n%s\ngot:\n%s", name, wantC, gotC)
	}
}

// canonicalize re-marshals JSON via encoding/json with the standard
// formatting we use, so trivial whitespace differences in the golden
// don't break the test.
func canonicalize(t *testing.T, raw []byte) []byte {
	t.Helper()
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("canonicalize unmarshal: %v", err)
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("canonicalize marshal: %v", err)
	}
	return out
}
