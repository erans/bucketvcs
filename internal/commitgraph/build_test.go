package commitgraph

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// skipIfNoGitB skips the test when git is not available.
// Named skipIfNoGitB to avoid collision with pack package's skipIfNoGit.
func skipIfNoGitB(t *testing.T) {
	t.Helper()
	if _, err := gitcli.Version(context.Background()); err != nil {
		t.Skip("git not available:", err)
	}
}

// openPackFromFiles uploads pack+idx to a fresh localfs store and returns a
// *pack.Reader.
func openPackFromFiles(t *testing.T, packPath, idxPath string) *pack.Reader {
	t.Helper()
	store, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatalf("localfs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	packData, err := os.ReadFile(packPath)
	if err != nil {
		t.Fatalf("ReadFile pack: %v", err)
	}
	idxData, err := os.ReadFile(idxPath)
	if err != nil {
		t.Fatalf("ReadFile idx: %v", err)
	}

	if _, err := store.PutIfAbsent(context.Background(), "p.pack", bytes.NewReader(packData), nil); err != nil {
		t.Fatalf("PutIfAbsent p.pack: %v", err)
	}
	if _, err := store.PutIfAbsent(context.Background(), "p.idx", bytes.NewReader(idxData), nil); err != nil {
		t.Fatalf("PutIfAbsent p.idx: %v", err)
	}

	r, err := pack.Open(context.Background(), store, "p.pack", "p.idx")
	if err != nil {
		t.Fatalf("pack.Open: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// buildPackRepo runs git commands in a temp worktree, clones to bare, packs,
// and returns a *pack.Reader.
func buildPackRepo(t *testing.T, setup func(mustGit func(...string) string)) *pack.Reader {
	t.Helper()
	skipIfNoGitB(t)

	work := t.TempDir()
	mustGit := func(args ...string) string {
		t.Helper()
		out, err := gitcli.RunForTest(work, args...)
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return trimNL(string(out))
	}
	mustGit("init", "--initial-branch=main")
	setup(mustGit)

	bareDir := filepath.Join(t.TempDir(), "bare")
	if err := gitcli.CloneBareMirror(context.Background(), work, bareDir); err != nil {
		t.Fatalf("CloneBareMirror: %v", err)
	}
	outDir := t.TempDir()
	prefix := filepath.Join(outDir, "pack")
	packID, err := gitcli.PackObjectsAll(context.Background(), bareDir, prefix)
	if err != nil {
		t.Fatalf("PackObjectsAll: %v", err)
	}
	return openPackFromFiles(t, prefix+"-"+packID+".pack", prefix+"-"+packID+".idx")
}

// trimNL removes trailing CR/LF characters.
func trimNL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// mustParseOIDStr parses a 40-char hex OID or fatals.
func mustParseOIDStr(t *testing.T, hex string) pack.OID {
	t.Helper()
	o, err := pack.ParseOID(hex)
	if err != nil {
		t.Fatalf("ParseOID(%q): %v", hex, err)
	}
	return o
}

// newLinearPackABC creates a linear chain A -> B -> C (C is the tip, A is root).
// gen(A)=1, gen(B)=2, gen(C)=3.
func newLinearPackABC(t *testing.T) (r *pack.Reader, oidA, oidB, oidC pack.OID) {
	t.Helper()
	var aStr, bStr, cStr string
	r = buildPackRepo(t, func(mustGit func(...string) string) {
		mustGit("-c", "user.name=t", "-c", "user.email=t@e",
			"commit", "--allow-empty", "-m", "A")
		aStr = mustGit("rev-parse", "HEAD")

		mustGit("-c", "user.name=t", "-c", "user.email=t@e",
			"commit", "--allow-empty", "-m", "B")
		bStr = mustGit("rev-parse", "HEAD")

		mustGit("-c", "user.name=t", "-c", "user.email=t@e",
			"commit", "--allow-empty", "-m", "C")
		cStr = mustGit("rev-parse", "HEAD")
	})
	return r, mustParseOIDStr(t, aStr), mustParseOIDStr(t, bStr), mustParseOIDStr(t, cStr)
}

// newOctopusPack creates a merge topology where M has parents P1 (gen=2),
// P2 (gen=4), P3 (gen=3), so gen(M) = 1 + max(2,4,3) = 5.
//
//	R (gen=1)
//	P1 -> R (gen=2)
//	Q  -> P1 (gen=3)   [intermediate, makes P2 deeper]
//	P2 -> Q (gen=4)
//	P3 -> P1 (gen=3)
//	M  -> P1, P2, P3 (gen=5)
func newOctopusPack(t *testing.T) (r *pack.Reader, oidM pack.OID) {
	t.Helper()
	var mStr string
	r = buildPackRepo(t, func(mustGit func(...string) string) {
		// R: root (gen=1)
		mustGit("-c", "user.name=t", "-c", "user.email=t@e",
			"commit", "--allow-empty", "-m", "R")

		// P1: child of R (gen=2)
		mustGit("checkout", "-b", "branch-p1")
		mustGit("-c", "user.name=t", "-c", "user.email=t@e",
			"commit", "--allow-empty", "-m", "P1")
		p1Str := mustGit("rev-parse", "HEAD")

		// Q: child of P1 (gen=3), used to build P2
		mustGit("checkout", "-b", "branch-q")
		mustGit("-c", "user.name=t", "-c", "user.email=t@e",
			"commit", "--allow-empty", "-m", "Q")

		// P2: child of Q (gen=4)
		mustGit("checkout", "-b", "branch-p2")
		mustGit("-c", "user.name=t", "-c", "user.email=t@e",
			"commit", "--allow-empty", "-m", "P2")
		p2Str := mustGit("rev-parse", "HEAD")

		// P3: child of P1 (gen=3)
		mustGit("checkout", p1Str)
		mustGit("checkout", "-b", "branch-p3")
		mustGit("-c", "user.name=t", "-c", "user.email=t@e",
			"commit", "--allow-empty", "-m", "P3")
		p3Str := mustGit("rev-parse", "HEAD")

		// M: octopus merge P1, P2, P3.  Checkout P1 first so P1 is
		// MERGE_HEAD[0] (first parent).
		mustGit("checkout", p1Str)
		mustGit("-c", "user.name=t", "-c", "user.email=t@e",
			"merge", "--allow-unrelated-histories", "--no-ff",
			"-m", "M",
			p2Str, p3Str)
		mStr = mustGit("rev-parse", "HEAD")

		// Update refs so CloneBareMirror picks up all branches.
		mustGit("update-ref", "refs/heads/branch-p1", p1Str)
		mustGit("update-ref", "refs/heads/branch-p2", p2Str)
		mustGit("update-ref", "refs/heads/branch-p3", p3Str)
		mustGit("update-ref", "refs/heads/branch-merge", mStr)
	})
	return r, mustParseOIDStr(t, mStr)
}

// readGenerationsByOID re-parses bvcg bytes to get commit records and reads
// generation numbers from the v2 wire format (or re-computes for v1).
func readGenerationsByOID(t *testing.T, b []byte) map[pack.OID]uint32 {
	t.Helper()
	records := parseRecords(t, b)
	return computeGenerations(records)
}

// parseRecords extracts commit records from bvcg bytes, handling both v1 and v2.
// Header (32 B): magic(4) + version(4, big-endian) + n_commits(8, big-endian) +
//
//	n_tips(4, big-endian) + reserved(12).
//
// Tips: n_tips × 24 B (ref_name_offset u32 + oid 20 B).
// v1 commits: oid(20) + n_parents(u8) + parents[n_parents]*20.
// v2 commits: oid(20) + gen(4 LE) + n_parents(u8) + parents[n_parents]*20.
func parseRecords(t *testing.T, b []byte) []Record {
	t.Helper()
	if len(b) < 32 {
		t.Fatalf("parseRecords: short header (%d bytes)", len(b))
	}
	ver := binary.BigEndian.Uint32(b[4:8])
	nCommits := binary.BigEndian.Uint64(b[8:16])
	nTips := binary.BigEndian.Uint32(b[16:20])

	off := 32 + int(nTips)*24 // skip header + tip table
	out := make([]Record, 0, nCommits)
	for i := uint64(0); i < nCommits; i++ {
		minSize := 21
		if ver >= VersionV2 {
			minSize = 25
		}
		if off+minSize > len(b) {
			t.Fatalf("parseRecords: commit %d truncated at offset %d", i, off)
		}
		var oid pack.OID
		copy(oid[:], b[off:off+20])
		off += 20

		var gen uint32
		if ver >= VersionV2 {
			gen = binary.LittleEndian.Uint32(b[off : off+4])
			off += 4
		}

		nParents := int(b[off])
		off++
		parents := make([]pack.OID, nParents)
		for j := 0; j < nParents; j++ {
			if off+20 > len(b) {
				t.Fatalf("parseRecords: parent %d of commit %d truncated", j, i)
			}
			copy(parents[j][:], b[off:off+20])
			off += 20
		}
		out = append(out, Record{OID: oid, Generation: gen, Parents: parents})
	}
	return out
}

// --- Tests ---

func TestBuild_GenerationNumbers_Linear(t *testing.T) {
	// Linear chain: A -> B -> C (C is the tip; A is the root).
	// gen(A) = 1, gen(B) = 2, gen(C) = 3.
	r, oidA, oidB, oidC := newLinearPackABC(t)
	tips := []Tip{{Ref: "refs/heads/main", OID: oidC}}
	out, err := Build(context.Background(), r, tips)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	gens := readGenerationsByOID(t, out)
	if gens[oidA] != 1 {
		t.Errorf("gen(A) = %d, want 1", gens[oidA])
	}
	if gens[oidB] != 2 {
		t.Errorf("gen(B) = %d, want 2", gens[oidB])
	}
	if gens[oidC] != 3 {
		t.Errorf("gen(C) = %d, want 3", gens[oidC])
	}
}

func TestBuild_GenerationNumbers_OctopusMerge(t *testing.T) {
	// Octopus: M has parents P1 (gen=2), P2 (gen=4), P3 (gen=3).
	// gen(M) = 1 + max(2,4,3) = 5.
	r, oidM := newOctopusPack(t)
	tips := []Tip{{Ref: "refs/heads/branch-merge", OID: oidM}}
	out, err := Build(context.Background(), r, tips)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	gens := readGenerationsByOID(t, out)
	if gens[oidM] != 5 {
		t.Errorf("gen(M) = %d, want 5", gens[oidM])
	}
}

// newRandomDAGPack generates a random DAG of n commits. Commit i picks 0-2
// parents from commits [0, i) in random order. Returns the resulting pack reader
// and the list of OIDs in creation order (index i = OID of commit i).
func newRandomDAGPack(t *testing.T, rng *rand.Rand, n int) (*pack.Reader, []pack.OID) {
	t.Helper()
	skipIfNoGitB(t)

	work := t.TempDir()
	mustGit := func(args ...string) string {
		t.Helper()
		out, err := gitcli.RunForTest(work, args...)
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return trimNL(string(out))
	}
	mustGit("init", "--initial-branch=main")

	oids := make([]pack.OID, 0, n)

	// Commit 0: root (no parents).
	mustGit("-c", "user.name=t", "-c", "user.email=t@e",
		"commit", "--allow-empty", "-m", "c0")
	c0Str := mustGit("rev-parse", "HEAD")
	c0OID := mustParseOIDStr(t, c0Str)
	oids = append(oids, c0OID)
	mustGit("update-ref", "refs/heads/c0", c0Str)

	// Commits 1..n-1: pick 0-2 parents from [0, i).
	for i := 1; i < n; i++ {
		// Randomly decide how many parents: 0, 1, or 2 (capped at available commits).
		maxParents := i
		if maxParents > 2 {
			maxParents = 2
		}
		numParents := rng.Intn(maxParents + 1) // 0 to maxParents inclusive
		var parentIndices []int

		// Pick unique parent indices from [0, i).
		if numParents > 0 {
			availableIndices := make([]int, i)
			for j := 0; j < i; j++ {
				availableIndices[j] = j
			}
			rng.Shuffle(len(availableIndices), func(a, b int) {
				availableIndices[a], availableIndices[b] = availableIndices[b], availableIndices[a]
			})
			for p := 0; p < numParents; p++ {
				parentIndices = append(parentIndices, availableIndices[p])
			}
		}

		if numParents == 0 {
			// Root or unrelated commit (rare in practice, but allowed).
			mustGit("-c", "user.name=t", "-c", "user.email=t@e",
				"commit", "--allow-empty", "-m", fmt.Sprintf("c%d", i))
		} else if numParents == 1 {
			// Single parent: just commit on top.
			parentIdx := parentIndices[0]
			parentOID := oids[parentIdx]
			parentStr := parentOID.String()
			mustGit("checkout", parentStr)
			mustGit("-c", "user.name=t", "-c", "user.email=t@e",
				"commit", "--allow-empty", "-m", fmt.Sprintf("c%d", i))
		} else {
			// Two parents: create a merge commit.
			// Checkout first parent, then merge the second.
			p1Idx := parentIndices[0]
			p2Idx := parentIndices[1]
			p1OID := oids[p1Idx]
			p2OID := oids[p2Idx]
			p1Str := p1OID.String()
			p2Str := p2OID.String()
			mustGit("checkout", p1Str)
			mustGit("-c", "user.name=t", "-c", "user.email=t@e",
				"merge", "--allow-unrelated-histories", "--no-ff",
				"-m", fmt.Sprintf("c%d", i),
				p2Str)
		}

		ciStr := mustGit("rev-parse", "HEAD")
		ciOID := mustParseOIDStr(t, ciStr)
		oids = append(oids, ciOID)
		mustGit("update-ref", fmt.Sprintf("refs/heads/c%d", i), ciStr)
	}

	// Create a bare clone and pack it.
	bareDir := filepath.Join(t.TempDir(), "bare")
	if err := gitcli.CloneBareMirror(context.Background(), work, bareDir); err != nil {
		t.Fatalf("CloneBareMirror: %v", err)
	}
	outDir := t.TempDir()
	prefix := filepath.Join(outDir, "pack")
	packID, err := gitcli.PackObjectsAll(context.Background(), bareDir, prefix)
	if err != nil {
		t.Fatalf("PackObjectsAll: %v", err)
	}
	r := openPackFromFiles(t, prefix+"-"+packID+".pack", prefix+"-"+packID+".idx")
	return r, oids
}

func TestBuild_GenerationProperty_RandomDAG(t *testing.T) {
	// Generate a random DAG of N commits; each commit picks 0..2
	// parents from already-emitted commits (in topological order).
	// Build .bvcg v2, read back via Reader, and verify
	//   gen(c) = 1 + max(gen(parents))  for every c.
	const N = 200
	rng := rand.New(rand.NewSource(1))
	r, oids := newRandomDAGPack(t, rng, N)
	tips := []Tip{{Ref: "refs/heads/main", OID: oids[N-1]}}
	bts, err := Build(context.Background(), r, tips)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	rdr, err := Open(bts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, oid := range oids {
		rec, ok := rdr.RecordOf(oid)
		if !ok {
			t.Errorf("missing oid %x", oid[:4])
			continue
		}
		var maxParent uint32
		for _, p := range rec.Parents {
			if pr, ok := rdr.RecordOf(p); ok && pr.Generation > maxParent {
				maxParent = pr.Generation
			}
		}
		want := maxParent + 1
		if rec.Generation != want {
			t.Errorf("oid %x: gen=%d, want %d (parents=%v)", oid[:4], rec.Generation, want, rec.Parents)
		}
	}
}
