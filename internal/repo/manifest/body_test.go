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
				Key:  "tenants/t/repos/r/indexes/commit-graph/dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd.bvcg",
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
			want := bytes.TrimRight(data, "\n")
			if !bytes.Equal(want, got) {
				t.Fatalf("round-trip byte mismatch.\nwant:\n%s\ngot:\n%s", want, got)
			}
		})
	}
}

func TestIndexRef_SizeBytes_Roundtrip(t *testing.T) {
	ref := IndexRef{Key: "tenants/t/repos/r/indexes/object-map/aa.bvom", Hash: "aa", SizeBytes: 12345}
	body := Body{Indexes: Indexes{ObjectMap: &ref}}
	out, err := MarshalBody(body)
	if err != nil {
		t.Fatalf("MarshalBody: %v", err)
	}
	if !bytes.Contains(out, []byte(`"size_bytes"`)) {
		t.Fatalf("size_bytes key missing from marshalled JSON:\n%s", out)
	}
	var got Body
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Indexes.ObjectMap == nil || got.Indexes.ObjectMap.SizeBytes != 12345 {
		t.Fatalf("size_bytes lost: got %+v", got.Indexes.ObjectMap)
	}
}

func TestIndexRef_SizeBytes_OmittedWhenZero(t *testing.T) {
	body := Body{Indexes: Indexes{ObjectMap: &IndexRef{Key: "k", Hash: "h"}}}
	out, err := MarshalBody(body)
	if err != nil {
		t.Fatalf("MarshalBody: %v", err)
	}
	if bytes.Contains(out, []byte(`"size_bytes"`)) {
		t.Fatalf("size_bytes should be omitted when zero, got:\n%s", out)
	}
}

func TestReachability_Roundtrip(t *testing.T) {
	body := Body{
		DefaultBranch: "main",
		Refs:          map[string]string{},
		Indexes: Indexes{
			ObjectMap:   &IndexRef{Key: "ok", Hash: "oh"},
			CommitGraph: &IndexRef{Key: "ck", Hash: "ch"},
			Reachability: &ReachabilityRef{
				BaseManifest: "v00000042",
				Deltas: []IndexRef{
					{Key: "d1k", Hash: "d1h", SizeBytes: 1024},
					{Key: "d2k", Hash: "d2h", SizeBytes: 2048},
				},
			},
		},
	}
	out, err := MarshalBody(body)
	if err != nil {
		t.Fatalf("MarshalBody: %v", err)
	}
	var got Body
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Indexes.Reachability == nil {
		t.Fatalf("Reachability lost")
	}
	if got.Indexes.Reachability.BaseManifest != "v00000042" {
		t.Fatalf("BaseManifest got %q", got.Indexes.Reachability.BaseManifest)
	}
	if len(got.Indexes.Reachability.Deltas) != 2 {
		t.Fatalf("Deltas len=%d", len(got.Indexes.Reachability.Deltas))
	}
}

func TestReachability_OmittedByDefault(t *testing.T) {
	body := Body{Indexes: Indexes{}}
	out, err := MarshalBody(body)
	if err != nil {
		t.Fatalf("MarshalBody: %v", err)
	}
	if bytes.Contains(out, []byte(`"reachability"`)) {
		t.Fatalf("reachability should be omitted when nil")
	}
}

func TestReachability_Deltas_NormalizedToEmptyArray(t *testing.T) {
	body := Body{Indexes: Indexes{Reachability: &ReachabilityRef{BaseManifest: "v00000001"}}}
	out, err := MarshalBody(body)
	if err != nil {
		t.Fatalf("MarshalBody: %v", err)
	}
	if bytes.Contains(out, []byte(`"deltas": null`)) {
		t.Fatalf("Deltas should normalize to [], got JSON containing null:\n%s", out)
	}
	if !bytes.Contains(out, []byte(`"deltas": []`)) {
		t.Fatalf("Deltas missing or wrong form, got JSON:\n%s", out)
	}
}

func TestReachability_NormalizationDoesNotMutateCaller(t *testing.T) {
	r := &ReachabilityRef{BaseManifest: "v1"}
	body := Body{Indexes: Indexes{Reachability: r}}
	if _, err := MarshalBody(body); err != nil {
		t.Fatalf("MarshalBody: %v", err)
	}
	if r.Deltas != nil {
		t.Fatalf("MarshalBody mutated caller's Deltas (was nil, now %v)", r.Deltas)
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
		if err := os.WriteFile(path, append(got, '\n'), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	want = bytes.TrimRight(want, "\n")
	// Strict byte-for-byte comparison enforces the documented wire-format
	// contract (2-space indent, key order, trailing newline...).
	if !bytes.Equal(want, got) {
		t.Fatalf("golden bytes mismatch %s.\nwant:\n%s\ngot:\n%s", name, want, got)
	}
}
