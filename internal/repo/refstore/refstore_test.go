package refstore_test

import (
	"encoding/json"
	"errors"
	"strconv"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo/refstore"
)

// TestStage_Lookup_Inline_Hit verifies inline Stage.Lookup returns the
// staged value for a ref present in NewInlineRefs.
func TestStage_Lookup_Inline_Hit(t *testing.T) {
	s := refstore.Stage{
		Mode: refstore.ModeInline,
		NewInlineRefs: map[string]string{
			"refs/heads/main": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}
	oid, exists, err := s.Lookup("refs/heads/main")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !exists {
		t.Fatalf("exists=false, want true")
	}
	if oid != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("oid=%q, want aaaa..", oid)
	}
}

// TestStage_Lookup_Inline_Miss verifies inline Stage.Lookup returns
// exists=false (and nil error — NOT ErrLookupNotInStage) for a ref
// absent from NewInlineRefs. Inline mode covers the full ref space,
// so absence is authoritative.
func TestStage_Lookup_Inline_Miss(t *testing.T) {
	s := refstore.Stage{
		Mode: refstore.ModeInline,
		NewInlineRefs: map[string]string{
			"refs/heads/main": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}
	_, exists, err := s.Lookup("refs/heads/nope")
	if err != nil {
		t.Fatalf("Lookup: %v (must be nil for inline mode)", err)
	}
	if exists {
		t.Fatalf("exists=true, want false")
	}
}

// TestStage_Lookup_Sharded_Hit verifies sharded Stage.Lookup finds a
// ref present in NewShardObjects: it must parse the shard contents and
// return the staged value.
func TestStage_Lookup_Sharded_Hit(t *testing.T) {
	refname := "refs/heads/main"
	sid := refstore.ShardKey(refname)
	contents, err := json.Marshal(map[string]string{
		refname: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := refstore.Stage{
		Mode: refstore.ModeSharded,
		NewShardObjects: []refstore.ShardWrite{
			{
				Shard:    sid,
				Key:      "manifest/ref-shards/sha256-xxx.json",
				Hash:     "sha256-xxx",
				Contents: contents,
			},
		},
	}
	oid, exists, err := s.Lookup(refname)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !exists {
		t.Fatalf("exists=false, want true")
	}
	if oid != "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("oid=%q, want bbbb..", oid)
	}
}

// TestStage_Lookup_Sharded_ShardWriteMissesRef verifies sharded
// Stage.Lookup returns exists=false (with nil error) when the
// refname's shard IS in NewShardObjects but the ref itself is not in
// that shard's contents — i.e., the ref was deleted by this stage.
func TestStage_Lookup_Sharded_ShardWriteMissesRef(t *testing.T) {
	deletedRef := "refs/heads/deleted"
	sid := refstore.ShardKey(deletedRef)
	// The shard contents contain SOME OTHER ref in the same shard
	// bucket — modeling "the deletion stage rewrote this shard without
	// the deleted ref".
	otherRef := refNameInSameShard(t, sid, "refs/heads/other")
	contents, err := json.Marshal(map[string]string{
		otherRef: "cccccccccccccccccccccccccccccccccccccccc",
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := refstore.Stage{
		Mode: refstore.ModeSharded,
		NewShardObjects: []refstore.ShardWrite{
			{
				Shard:    sid,
				Key:      "manifest/ref-shards/sha256-yyy.json",
				Hash:     "sha256-yyy",
				Contents: contents,
			},
		},
	}
	_, exists, err := s.Lookup(deletedRef)
	if err != nil {
		t.Fatalf("Lookup: %v (must be nil — shard was staged, just doesn't contain the ref)", err)
	}
	if exists {
		t.Fatalf("exists=true, want false (ref was deleted in this stage)")
	}
}

// TestStage_Lookup_Sharded_NotInStage verifies sharded Stage.Lookup
// returns ErrLookupNotInStage when the refname's shard is NOT in
// NewShardObjects — i.e., this stage didn't touch the shard, so the
// answer must come from the original RefStore.
func TestStage_Lookup_Sharded_NotInStage(t *testing.T) {
	untouchedRef := "refs/heads/untouched"
	untouchedSID := refstore.ShardKey(untouchedRef)

	// Stage writes a ShardWrite for a DIFFERENT shard.
	otherRef := refNameInDifferentShard(t, untouchedSID, "refs/heads/other")
	otherSID := refstore.ShardKey(otherRef)
	contents, err := json.Marshal(map[string]string{
		otherRef: "dddddddddddddddddddddddddddddddddddddddd",
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := refstore.Stage{
		Mode: refstore.ModeSharded,
		NewShardObjects: []refstore.ShardWrite{
			{
				Shard:    otherSID,
				Key:      "manifest/ref-shards/sha256-zzz.json",
				Hash:     "sha256-zzz",
				Contents: contents,
			},
		},
	}
	_, _, err = s.Lookup(untouchedRef)
	if !errors.Is(err, refstore.ErrLookupNotInStage) {
		t.Fatalf("Lookup err=%v, want ErrLookupNotInStage", err)
	}
}

// TestStage_Lookup_Sharded_ParseError verifies that a corrupted/
// malformed ShardWrite.Contents surfaces as a wrapped parse error
// rather than a silent miss. Stage is normally constructed only by
// trusted code (refstore.Stage method), but the public Lookup surface
// would be safer to surface parse errors explicitly.
func TestStage_Lookup_Sharded_ParseError(t *testing.T) {
	ref := "refs/heads/main"
	sid := refstore.ShardKey(ref)
	s := refstore.Stage{
		Mode: refstore.ModeSharded,
		NewShardObjects: []refstore.ShardWrite{
			{
				Shard:    sid,
				Key:      "manifest/ref-shards/sha256-zzz.json",
				Hash:     "sha256-zzz",
				Contents: []byte("not-json-at-all"),
			},
		},
	}
	_, _, err := s.Lookup(ref)
	if err == nil {
		t.Fatalf("Lookup err=nil, want parse error")
	}
	if errors.Is(err, refstore.ErrLookupNotInStage) {
		t.Errorf("Lookup err=%v wrongly classified as ErrLookupNotInStage (should be parse failure)", err)
	}
}

// refNameInSameShard finds a refname that hashes to the same shard ID
// as want by appending an incrementing suffix to base. Used by tests
// that need two distinct refs in the same shard bucket.
func refNameInSameShard(t *testing.T, want, base string) string {
	t.Helper()
	for i := 0; i < 10000; i++ {
		candidate := base + "-" + strconv.Itoa(i)
		if refstore.ShardKey(candidate) == want {
			return candidate
		}
	}
	t.Fatalf("could not find refname in shard %s after 10000 tries", want)
	return ""
}

// refNameInDifferentShard finds a refname that hashes to a DIFFERENT
// shard than avoid.
func refNameInDifferentShard(t *testing.T, avoid, base string) string {
	t.Helper()
	for i := 0; i < 10000; i++ {
		candidate := base + "-" + strconv.Itoa(i)
		if refstore.ShardKey(candidate) != avoid {
			return candidate
		}
	}
	t.Fatalf("could not find refname outside shard %s after 10000 tries", avoid)
	return ""
}
