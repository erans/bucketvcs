package pack

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := gitcli.Version(context.Background()); err != nil {
		t.Skip("git not available:", err)
	}
}

// makeOnePackRepo authors a small repo and produces a single pack via
// gitcli.PackObjectsAll. Returns the prefix passed to PackObjectsAll,
// the pack_id, and the bare repo path used to build it.
func makeOnePackRepo(t *testing.T) (prefix, packID, bareDir string) {
	t.Helper()
	skipIfNoGit(t)
	work := t.TempDir()
	mustGit := func(args ...string) {
		t.Helper()
		out, err := gitcli.RunForTest(work, args...)
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	mustGit("init", "--initial-branch=main")
	for _, msg := range []string{"a\n", "b\n", "c\n"} {
		if err := os.WriteFile(filepath.Join(work, "f"), []byte(msg), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		mustGit("add", "f")
		mustGit("-c", "user.name=t", "-c", "user.email=t@e",
			"commit", "-m", msg)
	}
	bareDir = filepath.Join(t.TempDir(), "bare")
	if err := gitcli.CloneBareMirror(context.Background(), work, bareDir); err != nil {
		t.Fatalf("CloneBareMirror: %v", err)
	}
	out := t.TempDir()
	prefix = filepath.Join(out, "pack")
	id, err := gitcli.PackObjectsAll(context.Background(), bareDir, prefix)
	if err != nil {
		t.Fatalf("PackObjectsAll: %v", err)
	}
	return prefix, id, bareDir
}

func TestParseIdx_RoundTripFanoutAndCount(t *testing.T) {
	prefix, id, _ := makeOnePackRepo(t)
	idxBytes, err := os.ReadFile(prefix + "-" + id + ".idx")
	if err != nil {
		t.Fatalf("ReadFile idx: %v", err)
	}
	idx, err := ParseIdx(bytes.NewReader(idxBytes), int64(len(idxBytes)))
	if err != nil {
		t.Fatalf("ParseIdx: %v", err)
	}
	if idx.Count() == 0 {
		t.Fatalf("expected non-zero object count")
	}
	// Fanout invariant: fanout[255] == count.
	if idx.Fanout()[255] != uint32(idx.Count()) {
		t.Fatalf("fanout[255]=%d != count=%d", idx.Fanout()[255], idx.Count())
	}
	// Iteration is OID-sorted.
	var prev OID
	first := true
	for i := 0; i < idx.Count(); i++ {
		oid := idx.OIDAt(i)
		if !first {
			if bytes.Compare(oid[:], prev[:]) <= 0 {
				t.Fatalf("OIDs not strictly ascending at %d", i)
			}
		}
		prev = oid
		first = false
	}
}

func TestIdx_LookupReturnsOffset(t *testing.T) {
	prefix, id, _ := makeOnePackRepo(t)
	idxBytes, err := os.ReadFile(prefix + "-" + id + ".idx")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	idx, err := ParseIdx(bytes.NewReader(idxBytes), int64(len(idxBytes)))
	if err != nil {
		t.Fatalf("ParseIdx: %v", err)
	}
	for i := 0; i < idx.Count(); i++ {
		oid := idx.OIDAt(i)
		off, ok := idx.Lookup(oid)
		if !ok {
			t.Fatalf("Lookup miss for OID at index %d", i)
		}
		_ = off
	}
}

func TestIdx_LookupMiss(t *testing.T) {
	prefix, id, _ := makeOnePackRepo(t)
	idxBytes, err := os.ReadFile(prefix + "-" + id + ".idx")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	idx, err := ParseIdx(bytes.NewReader(idxBytes), int64(len(idxBytes)))
	if err != nil {
		t.Fatalf("ParseIdx: %v", err)
	}
	var bogus OID
	if _, ok := idx.Lookup(bogus); ok {
		t.Fatalf("expected miss for zero OID")
	}
}

func TestParseIdx_RejectsBadMagic(t *testing.T) {
	garbage := make([]byte, 8+1024+40)
	copy(garbage[:4], []byte{0x00, 0x00, 0x00, 0x00}) // bad magic
	if _, err := ParseIdx(bytes.NewReader(garbage), int64(len(garbage))); err == nil {
		t.Fatalf("expected ParseIdx to reject bad magic")
	}
}

func TestParseIdx_RejectsBadVersion(t *testing.T) {
	garbage := make([]byte, 8+1024+40)
	copy(garbage[:4], []byte{0xff, 0x74, 0x4f, 0x63}) // good magic
	garbage[7] = 99                                    // bad version
	if _, err := ParseIdx(bytes.NewReader(garbage), int64(len(garbage))); err == nil {
		t.Fatalf("expected ParseIdx to reject bad version")
	}
}
