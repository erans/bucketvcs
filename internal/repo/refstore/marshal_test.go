package refstore

import (
	"bytes"
	"strings"
	"testing"
)

func TestMarshalShardContent_Empty(t *testing.T) {
	got, err := marshalShardContent(nil)
	if err != nil {
		t.Fatalf("marshalShardContent(nil): %v", err)
	}
	if string(got) != "{}" {
		t.Errorf("got %q want %q", got, "{}")
	}
}

func TestMarshalShardContent_Deterministic(t *testing.T) {
	// Map iteration order in Go is randomized; the marshaller must
	// sort keys to produce byte-identical output across runs.
	refs := map[string]string{
		"refs/heads/main":  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"refs/heads/dev":   "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"refs/tags/v1.0.0": "cccccccccccccccccccccccccccccccccccccccc",
	}
	var first []byte
	for i := 0; i < 50; i++ {
		got, err := marshalShardContent(refs)
		if err != nil {
			t.Fatalf("marshalShardContent: %v", err)
		}
		if first == nil {
			first = got
			continue
		}
		if !bytes.Equal(first, got) {
			t.Fatalf("non-deterministic output:\n  first=%s\n  later=%s", first, got)
		}
	}
}

func TestMarshalShardContent_SortedKeys(t *testing.T) {
	refs := map[string]string{
		"refs/heads/z":   "11",
		"refs/heads/aaa": "22",
		"refs/heads/m":   "33",
	}
	got, err := marshalShardContent(refs)
	if err != nil {
		t.Fatalf("marshalShardContent: %v", err)
	}
	// Find each ref's position and assert lex order.
	posA := strings.Index(string(got), "refs/heads/aaa")
	posM := strings.Index(string(got), "refs/heads/m")
	posZ := strings.Index(string(got), "refs/heads/z")
	if !(posA < posM && posM < posZ) {
		t.Errorf("keys not sorted: aaa@%d m@%d z@%d", posA, posM, posZ)
	}
}

func TestMarshalShardContent_TwoSpaceIndent(t *testing.T) {
	refs := map[string]string{"refs/heads/main": "aa"}
	got, err := marshalShardContent(refs)
	if err != nil {
		t.Fatalf("marshalShardContent: %v", err)
	}
	if !strings.Contains(string(got), "\n  \"refs/heads/main\"") {
		t.Errorf("expected 2-space indent before key; got %s", got)
	}
}

func TestMarshalShardContent_NoTrailingNewline(t *testing.T) {
	refs := map[string]string{"refs/heads/main": "aa"}
	got, err := marshalShardContent(refs)
	if err != nil {
		t.Fatalf("marshalShardContent: %v", err)
	}
	if bytes.HasSuffix(got, []byte("\n")) {
		t.Errorf("unexpected trailing newline in %q", got)
	}
}

func TestHashShardContent_KnownVector(t *testing.T) {
	// Determinism: the empty-shard hash must be stable.
	got := hashShardContent([]byte("{}"))
	const want = "sha256-44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"
	const wantPrefix = "sha256-"
	if got[:len(wantPrefix)] != wantPrefix {
		t.Errorf("hash prefix = %q want %q", got[:len(wantPrefix)], wantPrefix)
	}
	if len(got) != len(wantPrefix)+64 {
		t.Errorf("hash length = %d want %d", len(got), len(wantPrefix)+64)
	}
	// "{}" hex of sha256 is well-known:
	// printf '%s' '{}' | sha256sum → 44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a
	if got != want {
		t.Errorf("got %s want %s", got, want)
	}
}
