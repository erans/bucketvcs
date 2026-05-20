package manifest_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/repoerrs"
)

func TestUnmarshalBody_V1_RoundTrip(t *testing.T) {
	raw := []byte(`{"default_branch":"refs/heads/main","refs":{"refs/heads/main":"abc"},"packs":[],"indexes":{},"bundles":[]}`)
	b, err := manifest.UnmarshalBody(raw)
	if err != nil {
		t.Fatalf("UnmarshalBody: %v", err)
	}
	if b.DefaultBranch != "refs/heads/main" {
		t.Errorf("DefaultBranch = %q", b.DefaultBranch)
	}
	if got := b.Refs["refs/heads/main"]; got != "abc" {
		t.Errorf("Refs[main] = %q want abc", got)
	}
	if len(b.RefShards) != 0 {
		t.Errorf("RefShards = %v want empty", b.RefShards)
	}
}

func TestUnmarshalBody_V2_RoundTrip(t *testing.T) {
	raw := []byte(`{
  "default_branch": "refs/heads/main",
  "ref_shards": [
    {"shard":"00","key":"tenants/t/repos/r/manifest/ref-shards/sha256-aa.json","hash":"sha256-aa00000000000000000000000000000000000000000000000000000000000000","ref_count":1}
  ],
  "ref_sharding": "hash_v1",
  "packs": [],
  "indexes": {},
  "bundles": []
}`)
	b, err := manifest.UnmarshalBody(raw)
	if err != nil {
		t.Fatalf("UnmarshalBody: %v", err)
	}
	if b.RefSharding != "hash_v1" {
		t.Errorf("RefSharding = %q", b.RefSharding)
	}
	if len(b.RefShards) != 1 || b.RefShards[0].Shard != "00" {
		t.Errorf("RefShards = %+v", b.RefShards)
	}
	if len(b.Refs) != 0 {
		t.Errorf("Refs = %v want empty", b.Refs)
	}
}

func TestUnmarshalBody_RejectsHybrid(t *testing.T) {
	raw := []byte(`{"default_branch":"refs/heads/main","refs":{"refs/heads/main":"abc"},"ref_shards":[{"shard":"00","key":"k","hash":"sha256-aa00000000000000000000000000000000000000000000000000000000000000","ref_count":0}],"ref_sharding":"hash_v1","packs":[],"indexes":{},"bundles":[]}`)
	_, err := manifest.UnmarshalBody(raw)
	if !errors.Is(err, repoerrs.ErrInvalidManifest) {
		t.Fatalf("err = %v, want ErrInvalidManifest", err)
	}
}

func TestUnmarshalBody_RejectsUnknownSharding(t *testing.T) {
	raw := []byte(`{"default_branch":"refs/heads/main","ref_shards":[{"shard":"00","key":"k","hash":"sha256-aa00000000000000000000000000000000000000000000000000000000000000","ref_count":0}],"ref_sharding":"namespace_hash_v1","packs":[],"indexes":{},"bundles":[]}`)
	_, err := manifest.UnmarshalBody(raw)
	if !errors.Is(err, repoerrs.ErrInvalidManifest) {
		t.Fatalf("err = %v, want ErrInvalidManifest", err)
	}
}

func TestUnmarshalBody_RejectsBadShardID(t *testing.T) {
	for _, badShard := range []string{"", "0", "abc", "0g", "FF"} {
		raw := []byte(`{"default_branch":"refs/heads/main","ref_shards":[{"shard":"` + badShard + `","key":"k","hash":"sha256-aa00000000000000000000000000000000000000000000000000000000000000","ref_count":0}],"ref_sharding":"hash_v1","packs":[],"indexes":{},"bundles":[]}`)
		_, err := manifest.UnmarshalBody(raw)
		if !errors.Is(err, repoerrs.ErrInvalidManifest) {
			t.Errorf("shard=%q: err = %v, want ErrInvalidManifest", badShard, err)
		}
	}
}

func TestUnmarshalBody_RejectsDuplicateShard(t *testing.T) {
	raw := []byte(`{"default_branch":"refs/heads/main","ref_shards":[
{"shard":"00","key":"k1","hash":"sha256-aa00000000000000000000000000000000000000000000000000000000000000","ref_count":0},
{"shard":"00","key":"k2","hash":"sha256-bb00000000000000000000000000000000000000000000000000000000000000","ref_count":0}
],"ref_sharding":"hash_v1","packs":[],"indexes":{},"bundles":[]}`)
	_, err := manifest.UnmarshalBody(raw)
	if !errors.Is(err, repoerrs.ErrInvalidManifest) {
		t.Fatalf("err = %v, want ErrInvalidManifest", err)
	}
}

func TestUnmarshalBody_RejectsBadHash(t *testing.T) {
	// Wrong prefix.
	for _, bad := range []string{"sha1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "sha256-XYZ", "sha256-", "deadbeef"} {
		raw := []byte(`{"default_branch":"refs/heads/main","ref_shards":[{"shard":"00","key":"k","hash":"` + bad + `","ref_count":0}],"ref_sharding":"hash_v1","packs":[],"indexes":{},"bundles":[]}`)
		_, err := manifest.UnmarshalBody(raw)
		if !errors.Is(err, repoerrs.ErrInvalidManifest) {
			t.Errorf("hash=%q: err = %v, want ErrInvalidManifest", bad, err)
		}
	}
}

func TestUnmarshalBody_RejectsV2WithoutSharding(t *testing.T) {
	// RefShards populated but RefSharding empty.
	raw := []byte(`{"default_branch":"refs/heads/main","ref_shards":[{"shard":"00","key":"k","hash":"sha256-aa00000000000000000000000000000000000000000000000000000000000000","ref_count":0}],"packs":[],"indexes":{},"bundles":[]}`)
	_, err := manifest.UnmarshalBody(raw)
	if !errors.Is(err, repoerrs.ErrInvalidManifest) {
		t.Fatalf("err = %v, want ErrInvalidManifest", err)
	}
}

func TestUnmarshalBody_RejectsShardingWithoutShards(t *testing.T) {
	// RefSharding set but no RefShards. Treated as malformed.
	raw := []byte(`{"default_branch":"refs/heads/main","ref_sharding":"hash_v1","packs":[],"indexes":{},"bundles":[]}`)
	_, err := manifest.UnmarshalBody(raw)
	if !errors.Is(err, repoerrs.ErrInvalidManifest) {
		t.Fatalf("err = %v, want ErrInvalidManifest", err)
	}
}

func TestUnmarshalBody_V2_MultiShard(t *testing.T) {
	raw := []byte(`{
  "default_branch": "refs/heads/main",
  "ref_shards": [
    {"shard":"00","key":"k1","hash":"sha256-aa00000000000000000000000000000000000000000000000000000000000000","ref_count":1},
    {"shard":"7f","key":"k2","hash":"sha256-bb00000000000000000000000000000000000000000000000000000000000000","ref_count":3},
    {"shard":"ff","key":"k3","hash":"sha256-cc00000000000000000000000000000000000000000000000000000000000000","ref_count":2}
  ],
  "ref_sharding": "hash_v1",
  "packs": [], "indexes": {}, "bundles": []
}`)
	b, err := manifest.UnmarshalBody(raw)
	if err != nil {
		t.Fatalf("UnmarshalBody: %v", err)
	}
	if len(b.RefShards) != 3 {
		t.Fatalf("RefShards len=%d want 3", len(b.RefShards))
	}
	wantIDs := []string{"00", "7f", "ff"}
	for i, s := range b.RefShards {
		if s.Shard != wantIDs[i] {
			t.Errorf("RefShards[%d].Shard=%q want %q", i, s.Shard, wantIDs[i])
		}
	}
}

func TestUnmarshalBody_RejectsDuplicateHash(t *testing.T) {
	// Two distinct shards cannot legitimately share a content hash
	// (content-addressing). Reject as a tampering canary.
	raw := []byte(`{"default_branch":"refs/heads/main","ref_shards":[
{"shard":"00","key":"k1","hash":"sha256-aa00000000000000000000000000000000000000000000000000000000000000","ref_count":1},
{"shard":"ff","key":"k2","hash":"sha256-aa00000000000000000000000000000000000000000000000000000000000000","ref_count":1}
],"ref_sharding":"hash_v1","packs":[],"indexes":{},"bundles":[]}`)
	_, err := manifest.UnmarshalBody(raw)
	if !errors.Is(err, repoerrs.ErrInvalidManifest) {
		t.Fatalf("err = %v, want ErrInvalidManifest", err)
	}
}

func TestUnmarshalBody_V1_EmptyRepo(t *testing.T) {
	// A repo with zero refs and no shards is a legitimate v1 state.
	// Must NOT be rejected as "sharding without shards" or any other
	// invariant violation.
	raw := []byte(`{"default_branch":"refs/heads/main","packs":[],"indexes":{},"bundles":[]}`)
	b, err := manifest.UnmarshalBody(raw)
	if err != nil {
		t.Fatalf("UnmarshalBody: %v", err)
	}
	if len(b.Refs) != 0 || len(b.RefShards) != 0 || b.RefSharding != "" {
		t.Errorf("empty body parsed to %+v, want everything empty", b)
	}
}

func TestIsSupportedRefSharding(t *testing.T) {
	if !manifest.IsSupportedRefSharding("hash_v1") {
		t.Error("hash_v1 should be supported")
	}
	for _, bad := range []string{"", "namespace_hash_v1", "HASH_V1", "hash_v2"} {
		if manifest.IsSupportedRefSharding(bad) {
			t.Errorf("%q should NOT be supported", bad)
		}
	}
}

func TestUnmarshalBody_RejectsEmptyShardKey(t *testing.T) {
	raw := []byte(`{"default_branch":"refs/heads/main","ref_shards":[{"shard":"00","key":"","hash":"sha256-aa00000000000000000000000000000000000000000000000000000000000000","ref_count":0}],"ref_sharding":"hash_v1","packs":[],"indexes":{},"bundles":[]}`)
	_, err := manifest.UnmarshalBody(raw)
	if !errors.Is(err, repoerrs.ErrInvalidManifest) {
		t.Fatalf("err = %v, want ErrInvalidManifest", err)
	}
}

func TestUnmarshalBody_RejectsNegativeRefCount(t *testing.T) {
	raw := []byte(`{"default_branch":"refs/heads/main","ref_shards":[{"shard":"00","key":"k","hash":"sha256-aa00000000000000000000000000000000000000000000000000000000000000","ref_count":-1}],"ref_sharding":"hash_v1","packs":[],"indexes":{},"bundles":[]}`)
	_, err := manifest.UnmarshalBody(raw)
	if !errors.Is(err, repoerrs.ErrInvalidManifest) {
		t.Fatalf("err = %v, want ErrInvalidManifest", err)
	}
}

func TestUnmarshalBody_RejectsUnsortedShards(t *testing.T) {
	// Shards in descending order — violates ascending invariant.
	raw := []byte(`{"default_branch":"refs/heads/main","ref_shards":[
{"shard":"ff","key":"k1","hash":"sha256-aa00000000000000000000000000000000000000000000000000000000000000","ref_count":1},
{"shard":"00","key":"k2","hash":"sha256-bb00000000000000000000000000000000000000000000000000000000000000","ref_count":1}
],"ref_sharding":"hash_v1","packs":[],"indexes":{},"bundles":[]}`)
	_, err := manifest.UnmarshalBody(raw)
	if !errors.Is(err, repoerrs.ErrInvalidManifest) {
		t.Fatalf("err = %v, want ErrInvalidManifest", err)
	}
}

func TestUnmarshalBody_ParseErrorPreservesInnerError(t *testing.T) {
	// Malformed JSON — error should match both ErrInvalidManifest
	// (canonical sentinel) and the inner json.SyntaxError class
	// (so callers can do errors.As for diagnostics).
	raw := []byte(`{this-is-not-json`)
	_, err := manifest.UnmarshalBody(raw)
	if !errors.Is(err, repoerrs.ErrInvalidManifest) {
		t.Fatalf("err = %v, want ErrInvalidManifest", err)
	}
	var syntaxErr *json.SyntaxError
	if !errors.As(err, &syntaxErr) {
		t.Errorf("errors.As did not retrieve json.SyntaxError from %v", err)
	}
}

func TestMarshalBody_RejectsInvariantViolations(t *testing.T) {
	cases := []struct {
		name string
		body manifest.Body
	}{
		{
			"hybrid_v1_v2",
			manifest.Body{
				DefaultBranch: "refs/heads/main",
				Refs:          map[string]string{"refs/heads/main": "aa"},
				RefShards:     []manifest.RefShard{{Shard: "00", Key: "k", Hash: "sha256-aa00000000000000000000000000000000000000000000000000000000000000", RefCount: 1}},
				RefSharding:   "hash_v1",
			},
		},
		{
			"sharding_without_shards",
			manifest.Body{
				DefaultBranch: "refs/heads/main",
				RefSharding:   "hash_v1",
			},
		},
		{
			"unknown_sharding",
			manifest.Body{
				DefaultBranch: "refs/heads/main",
				RefShards:     []manifest.RefShard{{Shard: "00", Key: "k", Hash: "sha256-aa00000000000000000000000000000000000000000000000000000000000000", RefCount: 1}},
				RefSharding:   "namespace_hash_v1",
			},
		},
		{
			"bad_shard_id",
			manifest.Body{
				DefaultBranch: "refs/heads/main",
				RefShards:     []manifest.RefShard{{Shard: "FF", Key: "k", Hash: "sha256-aa00000000000000000000000000000000000000000000000000000000000000", RefCount: 0}},
				RefSharding:   "hash_v1",
			},
		},
		{
			"empty_shard_key",
			manifest.Body{
				DefaultBranch: "refs/heads/main",
				RefShards:     []manifest.RefShard{{Shard: "00", Key: "", Hash: "sha256-aa00000000000000000000000000000000000000000000000000000000000000", RefCount: 0}},
				RefSharding:   "hash_v1",
			},
		},
		{
			"negative_ref_count",
			manifest.Body{
				DefaultBranch: "refs/heads/main",
				RefShards:     []manifest.RefShard{{Shard: "00", Key: "k", Hash: "sha256-aa00000000000000000000000000000000000000000000000000000000000000", RefCount: -1}},
				RefSharding:   "hash_v1",
			},
		},
		{
			"unsorted_shards",
			manifest.Body{
				DefaultBranch: "refs/heads/main",
				RefShards: []manifest.RefShard{
					{Shard: "ff", Key: "k1", Hash: "sha256-aa00000000000000000000000000000000000000000000000000000000000000", RefCount: 1},
					{Shard: "00", Key: "k2", Hash: "sha256-bb00000000000000000000000000000000000000000000000000000000000000", RefCount: 1},
				},
				RefSharding: "hash_v1",
			},
		},
		{
			"duplicate_shard",
			manifest.Body{
				DefaultBranch: "refs/heads/main",
				RefShards: []manifest.RefShard{
					{Shard: "00", Key: "k1", Hash: "sha256-aa00000000000000000000000000000000000000000000000000000000000000", RefCount: 1},
					{Shard: "00", Key: "k2", Hash: "sha256-bb00000000000000000000000000000000000000000000000000000000000000", RefCount: 1},
				},
				RefSharding: "hash_v1",
			},
		},
		{
			"bad_hash",
			manifest.Body{
				DefaultBranch: "refs/heads/main",
				RefShards:     []manifest.RefShard{{Shard: "00", Key: "k", Hash: "sha1-deadbeef", RefCount: 1}},
				RefSharding:   "hash_v1",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := manifest.MarshalBody(c.body)
			if !errors.Is(err, repoerrs.ErrInvalidManifest) {
				t.Fatalf("MarshalBody: err = %v, want ErrInvalidManifest", err)
			}
		})
	}
}
