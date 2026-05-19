package gitcli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func skipIfNoGit(t *testing.T) {
	t.Helper()
	if _, err := Version(context.Background()); err != nil {
		t.Skip("git not available on PATH:", err)
	}
}

func TestVersion_Reports(t *testing.T) {
	skipIfNoGit(t)
	v, err := Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if !strings.HasPrefix(v, "git version ") {
		t.Fatalf("Version output unexpected: %q", v)
	}
}

func TestInitBare_CreatesObjectsDir(t *testing.T) {
	skipIfNoGit(t)
	dir := t.TempDir()
	if err := InitBare(context.Background(), dir); err != nil {
		t.Fatalf("InitBare: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "objects")); err != nil {
		t.Fatalf("expected objects/ dir after InitBare: %v", err)
	}
}

func TestFsck_OK(t *testing.T) {
	skipIfNoGit(t)
	dir := t.TempDir()
	if err := InitBare(context.Background(), dir); err != nil {
		t.Fatalf("InitBare: %v", err)
	}
	if err := Fsck(context.Background(), dir, true); err != nil {
		t.Fatalf("Fsck on empty bare repo: %v", err)
	}
}

func TestFsck_DetectsCorruption(t *testing.T) {
	skipIfNoGit(t)
	dir := t.TempDir()
	if err := InitBare(context.Background(), dir); err != nil {
		t.Fatalf("InitBare: %v", err)
	}
	// Drop a clearly-bogus loose object.
	bogus := filepath.Join(dir, "objects", "ab")
	if err := os.MkdirAll(bogus, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bogus, "cdef0123456789012345678901234567890123"), []byte("not-a-git-object"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := Fsck(context.Background(), dir, true); err == nil {
		t.Fatalf("expected Fsck to fail on corrupt loose object")
	}
}

func TestSetBinaryForTest_Override(t *testing.T) {
	old := SetBinaryForTest("/nonexistent-git-binary")
	t.Cleanup(func() { SetBinaryForTest(old) })
	if _, err := Version(context.Background()); err == nil {
		t.Fatalf("expected error when binary path is bogus")
	}
}

// TestRun_ScrubsRepoScopingEnv verifies that GIT_DIR set in the environment
// does not redirect InitBare away from the real target directory.
func TestRun_ScrubsRepoScopingEnv(t *testing.T) {
	skipIfNoGit(t)
	t.Setenv("GIT_DIR", "/some/bogus/path")
	dir := t.TempDir()
	if err := InitBare(context.Background(), dir); err != nil {
		t.Fatalf("InitBare with GIT_DIR set to bogus path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "objects")); err != nil {
		t.Fatalf("expected objects/ dir in real dir after InitBare: %v", err)
	}
}

// TestScrubGitRepoEnv_RemovesAllScopingVars verifies that the helper strips
// all 9 repo-scoping variables and leaves non-scoping variables intact.
func TestScrubGitRepoEnv_RemovesAllScopingVars(t *testing.T) {
	input := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/root",
		"GIT_AUTHOR_NAME=Alice",
		"GIT_DIR=/some/dir",
		"GIT_WORK_TREE=/work",
		"GIT_INDEX_FILE=/idx",
		"GIT_OBJECT_DIRECTORY=/obj",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=/alt",
		"GIT_COMMON_DIR=/common",
		"GIT_NAMESPACE=ns",
		"GIT_CEILING_DIRECTORIES=/ceiling",
		"GIT_DISCOVERY_ACROSS_FILESYSTEM=1",
	}
	got := scrubGitRepoEnv(input)

	// Non-scoping vars must survive.
	keep := []string{"PATH=/usr/bin:/bin", "HOME=/root", "GIT_AUTHOR_NAME=Alice"}
	for _, want := range keep {
		found := false
		for _, g := range got {
			if g == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("scrubGitRepoEnv removed %q but should have kept it", want)
		}
	}

	// Scoping vars must be absent.
	for _, scoping := range gitRepoScopingVars {
		for _, g := range got {
			key := g
			if idx := strings.Index(g, "="); idx >= 0 {
				key = g[:idx]
			}
			if key == scoping {
				t.Errorf("scrubGitRepoEnv kept scoping var %q", scoping)
			}
		}
	}
}

func makeRepoWithOneCommit(t *testing.T) string {
	t.Helper()
	skipIfNoGit(t)
	dir := t.TempDir()
	if err := InitBare(context.Background(), dir); err != nil {
		t.Fatalf("InitBare: %v", err)
	}
	// Use a non-bare working repo to author a commit, then clone --bare.
	work := t.TempDir()
	mustRun := func(args ...string) {
		t.Helper()
		bin, err := resolveBinary()
		if err != nil {
			t.Fatalf("resolveBinary: %v", err)
		}
		cmd := exec.Command(bin, append([]string{"-C", work}, args...)...)
		env := scrubGitRepoEnv(os.Environ())
		env = append(env,
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e",
		)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	mustRun("init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(work, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mustRun("add", "README")
	mustRun("commit", "-m", "init")
	// Clone --bare into a fresh temp dir. Use a named subdir so git creates
	// it (some git versions reject a pre-existing directory).
	out := filepath.Join(t.TempDir(), "bare")
	if err := CloneBareMirror(context.Background(), work, out); err != nil {
		t.Fatalf("CloneBareMirror: %v", err)
	}
	return out
}

func TestCloneBareMirror_PreservesRefs(t *testing.T) {
	skipIfNoGit(t)
	bare := makeRepoWithOneCommit(t)
	if _, err := os.Stat(filepath.Join(bare, "HEAD")); err != nil {
		t.Fatalf("expected HEAD: %v", err)
	}
	if _, err := os.Stat(filepath.Join(bare, "objects")); err != nil {
		t.Fatalf("expected objects/: %v", err)
	}
}

func TestPackObjectsAll_ProducesPackAndReturnsID(t *testing.T) {
	skipIfNoGit(t)
	bare := makeRepoWithOneCommit(t)
	outDir := t.TempDir()
	prefix := filepath.Join(outDir, "pack")
	id, err := PackObjectsAll(context.Background(), bare, prefix)
	if err != nil {
		t.Fatalf("PackObjectsAll: %v", err)
	}
	if len(id) != 40 {
		t.Fatalf("pack_id length: got %d, want 40 (%q)", len(id), id)
	}
	if _, err := os.Stat(prefix + "-" + id + ".pack"); err != nil {
		t.Fatalf("expected pack file: %v", err)
	}
	if _, err := os.Stat(prefix + "-" + id + ".idx"); err != nil {
		t.Fatalf("expected idx file: %v", err)
	}
}

func TestPackObjectsAllWithBitmap_ProducesBitmap(t *testing.T) {
	skipIfNoGit(t)
	bare := makeRepoWithOneCommit(t)
	outDir := t.TempDir()
	prefix := filepath.Join(outDir, "pack")
	id, err := PackObjectsAllWithBitmap(context.Background(), bare, prefix)
	if err != nil {
		t.Fatalf("PackObjectsAllWithBitmap: %v", err)
	}
	if len(id) != 40 {
		t.Fatalf("pack_id length: got %d, want 40 (%q)", len(id), id)
	}
	for _, ext := range []string{".pack", ".idx", ".bitmap"} {
		p := prefix + "-" + id + ext
		st, err := os.Stat(p)
		if err != nil {
			t.Errorf("missing %s: %v", ext, err)
			continue
		}
		if st.Size() == 0 {
			t.Errorf("%s is empty", ext)
		}
	}
}

func TestPackObjectsAllWithBitmap_EmptyRepoSucceedsWithEmptyHash(t *testing.T) {
	// Matches PackObjectsAll behavior: pack-objects on an empty repo
	// produces the well-known empty-pack hash (029d088...) and exits
	// cleanly. The bitmap variant inherits this — bitmap generation
	// on an empty pack is a no-op (no .bitmap file is written, but
	// the call still succeeds). Documenting the actual behavior here
	// so a future reader doesn't add an erroneous "must fail on empty"
	// assertion based on intuition.
	skipIfNoGit(t)
	dir := t.TempDir()
	if err := InitBare(context.Background(), dir); err != nil {
		t.Fatalf("InitBare: %v", err)
	}
	prefix := filepath.Join(t.TempDir(), "pack")
	id, err := PackObjectsAllWithBitmap(context.Background(), dir, prefix)
	if err != nil {
		t.Fatalf("PackObjectsAllWithBitmap on empty repo: %v", err)
	}
	if len(id) != 40 {
		t.Errorf("expected 40-char empty-pack hash, got %q", id)
	}
}

func TestIndexPack_ReindexesExistingPack(t *testing.T) {
	skipIfNoGit(t)
	bare := makeRepoWithOneCommit(t)
	tmp := t.TempDir()
	prefix := filepath.Join(tmp, "p")
	id, err := PackObjectsAll(context.Background(), bare, prefix)
	if err != nil {
		t.Fatalf("PackObjectsAll: %v", err)
	}
	// Remove .idx, reindex with IndexPack.
	idxPath := prefix + "-" + id + ".idx"
	packPath := prefix + "-" + id + ".pack"
	if err := os.Remove(idxPath); err != nil {
		t.Fatalf("Remove idx: %v", err)
	}
	if err := IndexPack(context.Background(), tmp, packPath); err != nil {
		t.Fatalf("IndexPack: %v", err)
	}
	if _, err := os.Stat(idxPath); err != nil {
		t.Fatalf("expected idx after IndexPack: %v", err)
	}
}

func TestUnpackObjects_ExplodesPackToLoose(t *testing.T) {
	skipIfNoGit(t)
	bare := makeRepoWithOneCommit(t)
	tmp := t.TempDir()
	prefix := filepath.Join(tmp, "p")
	id, err := PackObjectsAll(context.Background(), bare, prefix)
	if err != nil {
		t.Fatalf("PackObjectsAll: %v", err)
	}
	// Init a fresh bare repo, then unpack the pack into it.
	dst := t.TempDir()
	if err := InitBare(context.Background(), dst); err != nil {
		t.Fatalf("InitBare: %v", err)
	}
	packPath := prefix + "-" + id + ".pack"
	if err := UnpackObjects(context.Background(), dst, packPath); err != nil {
		t.Fatalf("UnpackObjects: %v", err)
	}
	// Loose objects appear under objects/<2-hex>/<38-hex>. Walk objects/
	// and confirm at least one such file exists.
	objectsDir := filepath.Join(dst, "objects")
	var loose int
	err = filepath.Walk(objectsDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		// Skip pack/, info/.
		rel := p[len(objectsDir):]
		if strings.Contains(rel, "/pack/") || strings.Contains(rel, "/info/") {
			return nil
		}
		// Loose object filenames are 38 hex chars under a 2-hex parent.
		if len(info.Name()) == 38 {
			loose++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if loose == 0 {
		t.Fatalf("expected ≥1 loose object after UnpackObjects, got 0")
	}
	if err := Fsck(context.Background(), dst, true); err != nil {
		t.Fatalf("Fsck after UnpackObjects: %v", err)
	}
}

func TestRunForTest_ReturnsCombinedOutput(t *testing.T) {
	skipIfNoGit(t)
	bare := makeRepoWithOneCommit(t)
	out, err := RunForTest(bare, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("RunForTest: %v: %s", err, out)
	}
	got := strings.TrimSpace(string(out))
	if len(got) != 40 {
		t.Fatalf("rev-parse HEAD: got %q (len %d), want 40-char SHA", got, len(got))
	}
}

func TestShowRef_AfterCommit(t *testing.T) {
	skipIfNoGit(t)
	bare := makeRepoWithOneCommit(t)
	refs, err := ShowRef(context.Background(), bare)
	if err != nil {
		t.Fatalf("ShowRef: %v", err)
	}
	if len(refs) == 0 {
		t.Fatalf("expected at least one ref, got none")
	}
	for ref, oid := range refs {
		if !strings.HasPrefix(ref, "refs/") {
			t.Fatalf("ref does not start with refs/: %q", ref)
		}
		if len(oid) != 40 {
			t.Fatalf("oid length: got %d for %q", len(oid), ref)
		}
	}
}

func TestShowRef_EmptyRepo(t *testing.T) {
	skipIfNoGit(t)
	dir := t.TempDir()
	if err := InitBare(context.Background(), dir); err != nil {
		t.Fatalf("InitBare: %v", err)
	}
	refs, err := ShowRef(context.Background(), dir)
	if err != nil {
		t.Fatalf("ShowRef on empty repo: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("expected 0 refs on empty repo, got %d", len(refs))
	}
}

func TestSymbolicRef_HEAD(t *testing.T) {
	skipIfNoGit(t)
	bare := makeRepoWithOneCommit(t)
	target, err := SymbolicRef(context.Background(), bare, "HEAD")
	if err != nil {
		t.Fatalf("SymbolicRef: %v", err)
	}
	if !strings.HasPrefix(target, "refs/heads/") {
		t.Fatalf("HEAD target unexpected: %q", target)
	}
}

func TestSymbolicRefSet_PointsHEADAtBranch(t *testing.T) {
	skipIfNoGit(t)
	bare := makeRepoWithOneCommit(t)
	// Create a second branch via update-ref against the existing main tip.
	refs, err := ShowRef(context.Background(), bare)
	if err != nil {
		t.Fatalf("ShowRef: %v", err)
	}
	var tip string
	for _, v := range refs {
		tip = v
		break
	}
	if err := UpdateRef(context.Background(), bare, "refs/heads/dev", tip); err != nil {
		t.Fatalf("UpdateRef: %v", err)
	}
	if err := SymbolicRefSet(context.Background(), bare, "HEAD", "refs/heads/dev"); err != nil {
		t.Fatalf("SymbolicRefSet: %v", err)
	}
	got, err := SymbolicRef(context.Background(), bare, "HEAD")
	if err != nil {
		t.Fatalf("SymbolicRef: %v", err)
	}
	if got != "refs/heads/dev" {
		t.Fatalf("HEAD: got %q, want refs/heads/dev", got)
	}
}

func TestRevListAllObjects_NonEmpty(t *testing.T) {
	skipIfNoGit(t)
	bare := makeRepoWithOneCommit(t)
	oids, err := RevListAllObjects(context.Background(), bare)
	if err != nil {
		t.Fatalf("RevListAllObjects: %v", err)
	}
	// Single-commit repo: one commit, one tree, one blob => 3 objects.
	if len(oids) < 3 {
		t.Fatalf("expected ≥3 reachable objects, got %d: %v", len(oids), oids)
	}
	for _, oid := range oids {
		if len(oid) != 40 {
			t.Fatalf("oid length: got %d for %q", len(oid), oid)
		}
	}
}

func TestCatFilePretty_Commit(t *testing.T) {
	skipIfNoGit(t)
	bare := makeRepoWithOneCommit(t)
	refs, err := ShowRef(context.Background(), bare)
	if err != nil {
		t.Fatalf("ShowRef: %v", err)
	}
	var oid string
	for _, v := range refs {
		oid = v
		break
	}
	out, err := CatFilePretty(context.Background(), bare, oid)
	if err != nil {
		t.Fatalf("CatFilePretty: %v", err)
	}
	if !bytes.Contains(out, []byte("tree ")) {
		t.Fatalf("commit pretty output missing tree line: %q", out)
	}
}

func TestCatFileType_Commit(t *testing.T) {
	skipIfNoGit(t)
	bare := makeRepoWithOneCommit(t)
	refs, err := ShowRef(context.Background(), bare)
	if err != nil {
		t.Fatalf("ShowRef: %v", err)
	}
	var oid string
	for _, v := range refs {
		oid = v
		break
	}
	got, err := CatFileType(context.Background(), bare, oid)
	if err != nil {
		t.Fatalf("CatFileType: %v", err)
	}
	if got != "commit" {
		t.Fatalf("type: got %q, want commit", got)
	}
}

func TestCatFileSize_Commit(t *testing.T) {
	skipIfNoGit(t)
	bare := makeRepoWithOneCommit(t)
	refs, err := ShowRef(context.Background(), bare)
	if err != nil {
		t.Fatalf("ShowRef: %v", err)
	}
	var oid string
	for _, v := range refs {
		oid = v
		break
	}
	n, err := CatFileSize(context.Background(), bare, oid)
	if err != nil {
		t.Fatalf("CatFileSize: %v", err)
	}
	if n <= 0 {
		t.Fatalf("size: got %d, want > 0", n)
	}
}

func TestCatFileSize_RejectsBadOID(t *testing.T) {
	skipIfNoGit(t)
	bare := makeRepoWithOneCommit(t)
	if _, err := CatFileSize(context.Background(), bare,
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"); err == nil {
		t.Fatalf("expected error for nonexistent oid")
	}
}

func TestRedactCreds_StripsURLUserInfo(t *testing.T) {
	cases := map[string]string{
		// user:password
		"https://user:token@github.com/x.git": "https://REDACTED@github.com/x.git",
		"http://alice:hunter2@host":           "http://REDACTED@host",
		// token-only
		"https://ghp_TOKEN@github.com/x.git":                "https://REDACTED@github.com/x.git",
		"https://x-access-token:ghp_TOKEN@github.com/x.git": "https://REDACTED@github.com/x.git",
		// embedded in surrounding text
		"clone failed: https://u:p@h/r.git failed":   "clone failed: https://REDACTED@h/r.git failed",
		"clone failed: https://TOKEN@h/r.git failed": "clone failed: https://REDACTED@h/r.git failed",
		// no scheme = unchanged
		"local-path/repo.git": "local-path/repo.git",
		// scheme without userinfo = unchanged
		"https://github.com/x.git": "https://github.com/x.git",
	}
	for in, want := range cases {
		if got := redactCreds(in); got != want {
			t.Errorf("redactCreds(%q): got %q, want %q", in, got, want)
		}
	}
}

func TestUpdateRef_CreatesBranch(t *testing.T) {
	skipIfNoGit(t)
	bare := makeRepoWithOneCommit(t)
	refs, err := ShowRef(context.Background(), bare)
	if err != nil {
		t.Fatalf("ShowRef: %v", err)
	}
	var tip string
	for _, v := range refs {
		tip = v
		break
	}
	if tip == "" {
		t.Fatalf("expected at least one ref tip in fixture")
	}
	if err := UpdateRef(context.Background(), bare, "refs/heads/dev", tip); err != nil {
		t.Fatalf("UpdateRef: %v", err)
	}
	got, err := ShowRef(context.Background(), bare)
	if err != nil {
		t.Fatalf("ShowRef: %v", err)
	}
	devOID, ok := got["refs/heads/dev"]
	if !ok {
		t.Fatalf("refs/heads/dev not present after UpdateRef")
	}
	if devOID != tip {
		t.Fatalf("refs/heads/dev: got %s, want %s", devOID, tip)
	}
}

func TestPackObjectsAll_HandlesLargerOutput(t *testing.T) {
	skipIfNoGit(t)
	// Build a repo with ~100 small commits to force a non-trivial
	// rev-list | pack-objects stream. Ensures the pipeline doesn't
	// truncate under realistic load.
	work := t.TempDir()
	bin, err := resolveBinary()
	if err != nil {
		t.Fatalf("resolveBinary: %v", err)
	}
	mustGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command(bin, append([]string{"-C", work}, args...)...)
		cmd.Env = scrubGitRepoEnv(os.Environ())
		cmd.Env = append(cmd.Env,
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	mustGit("init", "--initial-branch=main")
	for i := 0; i < 50; i++ {
		path := filepath.Join(work, "f")
		// Vary BOTH file content AND commit message per iteration so
		// every commit produces a distinct tree+commit OID. Earlier
		// versions reused the message every 10 iterations, occasionally
		// triggering "unable to read <oid>" on rapid-fire commits with
		// the same git plumbing state.
		if err := os.WriteFile(path, []byte(fmt.Sprintf("rev=%d\n", i)), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		mustGit("add", "f")
		mustGit("commit", "-m", fmt.Sprintf("c%d", i))
	}
	bare := filepath.Join(t.TempDir(), "bare")
	if err := CloneBareMirror(context.Background(), work, bare); err != nil {
		t.Fatalf("CloneBareMirror: %v", err)
	}
	prefix := filepath.Join(t.TempDir(), "p")
	id, err := PackObjectsAll(context.Background(), bare, prefix)
	if err != nil {
		t.Fatalf("PackObjectsAll: %v", err)
	}
	if len(id) != 40 {
		t.Fatalf("pack_id length: %d", len(id))
	}
	// The pack should exist and be non-trivial in size.
	st, err := os.Stat(prefix + "-" + id + ".pack")
	if err != nil {
		t.Fatalf("stat pack: %v", err)
	}
	if st.Size() < 1000 {
		t.Fatalf("pack too small: %d bytes (truncation?)", st.Size())
	}
}

func TestPackObjectsAll_RedactsCredsInStderr(t *testing.T) {
	skipIfNoGit(t)
	// Force a failure with a URL containing credentials embedded in
	// the SOURCE path. PackObjectsAll itself doesn't take a URL — but
	// CloneBareMirror does. The cleaner test target is CloneBareMirror's
	// error path: try to clone from a bogus https URL with creds and
	// verify the returned error doesn't contain the cred substring.
	bogusURL := "https://user:supersecret@nonexistent.invalid/repo.git"
	dst := filepath.Join(t.TempDir(), "x")
	err := CloneBareMirror(context.Background(), bogusURL, dst)
	if err == nil {
		t.Fatalf("expected clone to fail")
	}
	if strings.Contains(err.Error(), "supersecret") {
		t.Fatalf("error message leaked credentials: %v", err)
	}
}

func TestValidRefOrOID(t *testing.T) {
	cases := map[string]bool{
		"refs/heads/main":                          true,
		"0123456789abcdef0123456789abcdef01234567": true,
		"--config=foo":                             false,
		"-rf":                                      false,
		"":                                         false,
		"with space":                               false,
		"with\ttab":                                false,
		"with\nnewline":                            false,
	}
	for in, want := range cases {
		got := validRefOrOID(in)
		if got != want {
			t.Errorf("validRefOrOID(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestUpdateRef_RejectsDashRef(t *testing.T) {
	skipIfNoGit(t)
	dir := t.TempDir()
	if err := InitBare(context.Background(), dir); err != nil {
		t.Fatalf("InitBare: %v", err)
	}
	if err := UpdateRef(context.Background(), dir, "--config=foo", "0123456789abcdef0123456789abcdef01234567"); err == nil {
		t.Fatalf("expected rejection of dash-prefixed ref")
	}
}

func TestCatFile_RejectsDashOID(t *testing.T) {
	skipIfNoGit(t)
	dir := t.TempDir()
	if err := InitBare(context.Background(), dir); err != nil {
		t.Fatalf("InitBare: %v", err)
	}
	if _, err := CatFilePretty(context.Background(), dir, "--config=foo"); err == nil {
		t.Fatalf("expected rejection of dash-prefixed oid")
	}
}

// mustGit is a top-level test helper that runs git with the given args in dir
// and fails the test on error.
func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	bin, err := resolveBinary()
	if err != nil {
		t.Fatalf("resolveBinary: %v", err)
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = scrubGitRepoEnv(os.Environ())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// mustGitCapture runs git with the given args in dir and returns stdout,
// failing the test on error.
func mustGitCapture(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	out, err := RunForTest(dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return out
}

func TestPackObjectsForFetch_RoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	dir := t.TempDir()
	bare := filepath.Join(dir, "src.git")

	mustGit(t, dir, "init", "--bare", bare)
	work := filepath.Join(dir, "wt")
	mustGit(t, dir, "clone", bare, work)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustGit(t, work, "add", ".")
	mustGit(t, work, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "init")
	mustGit(t, work, "push", "origin", "HEAD:refs/heads/main")
	tip := strings.TrimSpace(string(mustGitCapture(t, work, "rev-parse", "HEAD")))

	ctx := context.Background()
	out, err := PackObjectsForFetch(ctx, bare, PackForFetchOptions{
		Wants:      []string{tip},
		Haves:      nil,
		ThinPack:   true,
		IncludeTag: true,
		OfsDelta:   true,
	})
	if err != nil {
		t.Fatalf("PackObjectsForFetch: %v", err)
	}
	defer out.Close()

	dst := filepath.Join(dir, "dst.git")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("mkdir dst: %v", err)
	}
	if err := InitBare(ctx, dst); err != nil {
		t.Fatalf("InitBare: %v", err)
	}
	packDir := filepath.Join(dst, "objects", "pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	packPath := filepath.Join(packDir, "pack-test.pack")
	f, err := os.Create(packPath)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := io.Copy(f, out); err != nil {
		t.Fatalf("copy: %v", err)
	}
	f.Close()
	if err := IndexPack(ctx, dst, packPath); err != nil {
		t.Fatalf("IndexPack: %v", err)
	}
}

func TestRevParseObjectKind_CommitTagBlob(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	dir := t.TempDir()
	bare := filepath.Join(dir, "src.git")
	mustGit(t, dir, "init", "--bare", bare)
	work := filepath.Join(dir, "wt")
	mustGit(t, dir, "clone", bare, work)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustGit(t, work, "add", ".")
	mustGit(t, work, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "x")
	mustGit(t, work, "tag", "v1")
	mustGit(t, work, "push", "origin", "HEAD:refs/heads/main", "tag", "v1")

	ctx := context.Background()
	commitOID := strings.TrimSpace(string(mustGitCapture(t, work, "rev-parse", "HEAD")))
	tagOID := strings.TrimSpace(string(mustGitCapture(t, work, "rev-parse", "v1")))

	if k, err := RevParseObjectKind(ctx, bare, commitOID); err != nil || k != "commit" {
		t.Fatalf("commit: kind=%q err=%v", k, err)
	}
	if k, err := RevParseObjectKind(ctx, bare, tagOID); err != nil {
		t.Fatalf("tag: err=%v", err)
	} else if k != "commit" && k != "tag" {
		t.Fatalf("tag kind=%q", k)
	}
}

func TestPackObjectsForFetch_IncrementalWithHaves(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	dir := t.TempDir()
	bare := filepath.Join(dir, "src.git")
	mustGit(t, dir, "init", "--bare", bare)
	work := filepath.Join(dir, "wt")
	mustGit(t, dir, "clone", bare, work)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustGit(t, work, "add", ".")
	mustGit(t, work, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "v1")
	firstOID := strings.TrimSpace(string(mustGitCapture(t, work, "rev-parse", "HEAD")))
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustGit(t, work, "add", ".")
	mustGit(t, work, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "v2")
	secondOID := strings.TrimSpace(string(mustGitCapture(t, work, "rev-parse", "HEAD")))
	mustGit(t, work, "push", "origin", "HEAD:refs/heads/main")

	// Incremental fetch: have firstOID, want secondOID. The result should be
	// non-empty (at least the new commit and modified blob) and indexable.
	ctx := context.Background()
	out, err := PackObjectsForFetch(ctx, bare, PackForFetchOptions{
		Wants:    []string{secondOID},
		Haves:    []string{firstOID},
		ThinPack: true,
		OfsDelta: true,
	})
	if err != nil {
		t.Fatalf("PackObjectsForFetch: %v", err)
	}

	dst := filepath.Join(dir, "dst.git")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := InitBare(ctx, dst); err != nil {
		t.Fatalf("InitBare: %v", err)
	}
	packDir := filepath.Join(dst, "objects", "pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatalf("mkdir pack: %v", err)
	}
	packPath := filepath.Join(packDir, "pack-incr.pack")
	f, err := os.Create(packPath)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	n, err := io.Copy(f, out)
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if n == 0 {
		t.Fatalf("incremental pack was empty")
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close pack file: %v", err)
	}
	// Close() is the only place pack-objects exit status is surfaced —
	// call it explicitly (not deferred) and fail on non-clean exit.
	if err := out.Close(); err != nil {
		t.Fatalf("Close (pack-objects exit): %v", err)
	}
	// Thin packs need --fix-thin to be indexable into a fresh repo. We don't
	// have IndexPackStrict here yet (Task 10), so just verify nonzero size
	// and clean subprocess exit (asserted above).
}

func TestPackObjectsForFetch_BogusWantSurfacesError(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	dir := t.TempDir()
	bare := filepath.Join(dir, "src.git")
	mustGit(t, dir, "init", "--bare", bare)

	ctx := context.Background()
	out, err := PackObjectsForFetch(ctx, bare, PackForFetchOptions{
		Wants: []string{"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"},
	})
	if err != nil {
		// Acceptable: the start path may detect the bogus want and fail early.
		return
	}
	_, _ = io.Copy(io.Discard, out)
	if err := out.Close(); err == nil {
		t.Fatalf("Close: expected error for bogus want OID")
	}
}

func TestRevParseObjectKind_MissingOIDReturnsError(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	dir := t.TempDir()
	bare := filepath.Join(dir, "src.git")
	mustGit(t, dir, "init", "--bare", bare)
	if _, err := RevParseObjectKind(context.Background(), bare, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"); err == nil {
		t.Fatalf("RevParseObjectKind: expected error for missing oid")
	}
}

// TestRevParseObjectKind_RejectsNonHexInputs ensures RevParseObjectKind
// refuses ref names, short OIDs, and revision syntax — only strict hex
// OIDs are accepted, matching the godoc contract.
func TestRevParseObjectKind_RejectsNonHexInputs(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	dir := t.TempDir()
	bare := filepath.Join(dir, "src.git")
	mustGit(t, dir, "init", "--bare", bare)
	work := filepath.Join(dir, "wt")
	mustGit(t, dir, "clone", bare, work)
	if err := os.WriteFile(filepath.Join(work, "x.txt"), []byte("y"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustGit(t, work, "add", ".")
	mustGit(t, work, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "x")
	mustGit(t, work, "push", "origin", "HEAD:refs/heads/main")
	ctx := context.Background()
	for _, in := range []string{
		"",
		"HEAD",
		"main",
		"HEAD^{tree}",
		"HEAD:x.txt",
		"deadbeef",                          // short OID
		"-deadbeefdeadbeefdeadbeefdeadbeef", // dash-prefixed
		"DEADBEEFDEADBEEFDEADBEEFDEADBEEFDEADBEEF", // uppercase
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef\nmain",
	} {
		if _, err := RevParseObjectKind(ctx, bare, in); err == nil {
			t.Fatalf("RevParseObjectKind(%q): expected validation error, got nil", in)
		}
	}
}

func TestPackObjectsForFetch_ShallowFileForwardedAsConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	dir := t.TempDir()
	bare := filepath.Join(dir, "src.git")
	mustGit(t, dir, "init", "--bare", bare)
	work := filepath.Join(dir, "wt")
	mustGit(t, dir, "clone", bare, work)
	// Build A -> B -> C so we can prove the shallow boundary excludes
	// ancestors from the resulting pack.
	for i, txt := range []string{"a", "b", "c"} {
		if err := os.WriteFile(filepath.Join(work, "f.txt"), []byte(txt), 0o644); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		mustGit(t, work, "add", ".")
		mustGit(t, work, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", txt)
	}
	mustGit(t, work, "push", "origin", "HEAD:refs/heads/main")
	tip := strings.TrimSpace(string(mustGitCapture(t, work, "rev-parse", "HEAD")))

	// countCommits returns how many commit objects are present in the
	// pack stream produced by PackObjectsForFetch.
	countCommits := func(t *testing.T, opts PackForFetchOptions) int {
		t.Helper()
		ctx := context.Background()
		rc, err := PackObjectsForFetch(ctx, bare, opts)
		if err != nil {
			t.Fatalf("PackObjectsForFetch: %v", err)
		}
		packDir := t.TempDir()
		packPath := filepath.Join(packDir, "in.pack")
		f, err := os.Create(packPath)
		if err != nil {
			t.Fatalf("create pack: %v", err)
		}
		if _, err := io.Copy(f, rc); err != nil {
			t.Fatalf("copy pack: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("close pack file: %v", err)
		}
		if err := rc.Close(); err != nil {
			t.Fatalf("close pack reader: %v", err)
		}
		// Index the pack so we can list its objects.
		mustGit(t, packDir, "index-pack", "in.pack")
		out := mustGitCapture(t, packDir, "verify-pack", "-v", "in.pack")
		commits := 0
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[1] == "commit" {
				commits++
			}
		}
		return commits
	}

	// Baseline: no shallow file -> all 3 commits in the pack.
	full := countCommits(t, PackForFetchOptions{Wants: []string{tip}})
	if full != 3 {
		t.Fatalf("baseline pack: expected 3 commits, got %d", full)
	}

	// With a shallow file that marks the tip itself as shallow, history
	// is cut at C and pack-objects must NOT include C's ancestors. If
	// the shallow file is silently ignored (the prior `-c core.shallow=`
	// bug), the pack would still contain all 3 commits.
	shallow := filepath.Join(t.TempDir(), "shallow")
	if err := os.WriteFile(shallow, []byte(tip+"\n"), 0o644); err != nil {
		t.Fatalf("write shallow: %v", err)
	}
	got := countCommits(t, PackForFetchOptions{
		Wants:       []string{tip},
		ShallowFile: shallow,
	})
	if got != 1 {
		t.Fatalf("shallow pack: expected 1 commit (boundary at tip), got %d (shallow file likely ignored)", got)
	}
}

// TestPackObjectsForFetch_RejectsNonHexWantsAndHaves verifies that
// PackObjectsForFetch rejects untrusted-looking want/have values before
// invoking git, so newline injection, leading-dash flag injection, or
// revision syntax (`^`, `..`, `:`, `~`) cannot smuggle extra revs or
// exclusions into `git pack-objects --revs` stdin.
func TestPackObjectsForFetch_RejectsNonHexWantsAndHaves(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	dir := t.TempDir()
	bare := filepath.Join(dir, "src.git")
	mustGit(t, dir, "init", "--bare", bare)
	ctx := context.Background()

	cases := []struct {
		name string
		opts PackForFetchOptions
	}{
		{"empty want", PackForFetchOptions{Wants: []string{""}}},
		{"newline want", PackForFetchOptions{Wants: []string{"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef\nHEAD"}}},
		{"dash want", PackForFetchOptions{Wants: []string{"--all"}}},
		{"caret rev want", PackForFetchOptions{Wants: []string{"^deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"}}},
		{"branch name want", PackForFetchOptions{Wants: []string{"main"}}},
		{"short oid want", PackForFetchOptions{Wants: []string{"deadbeef"}}},
		{"uppercase want", PackForFetchOptions{Wants: []string{"DEADBEEFDEADBEEFDEADBEEFDEADBEEFDEADBEEF"}}},
		{"non-hex char want", PackForFetchOptions{Wants: []string{"zeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"}}},
		{"newline have", PackForFetchOptions{
			Wants: []string{"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"},
			Haves: []string{"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef\n--all"},
		}},
		{"dash have", PackForFetchOptions{
			Wants: []string{"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"},
			Haves: []string{"--shallow-exclude=main"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := PackObjectsForFetch(ctx, bare, tc.opts)
			if err == nil {
				if out != nil {
					_ = out.Close()
				}
				t.Fatalf("PackObjectsForFetch: expected validation error, got nil")
			}
		})
	}
}

func TestIndexPackStrict_RoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	dir := t.TempDir()
	bare := filepath.Join(dir, "src.git")
	mustGit(t, dir, "init", "--bare", bare)
	work := filepath.Join(dir, "wt")
	mustGit(t, dir, "clone", bare, work)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustGit(t, work, "add", ".")
	mustGit(t, work, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "x")
	mustGit(t, work, "push", "origin", "HEAD:refs/heads/main")
	tip := strings.TrimSpace(string(mustGitCapture(t, work, "rev-parse", "HEAD")))

	// Build a thin pack from src/bare into a tempfile.
	ctx := context.Background()
	r, err := PackObjectsForFetch(ctx, bare, PackForFetchOptions{Wants: []string{tip}, ThinPack: true, OfsDelta: true})
	if err != nil {
		t.Fatalf("PackObjectsForFetch: %v", err)
	}
	defer r.Close()
	tmp := filepath.Join(dir, "incoming.pack")
	f, err := os.Create(tmp)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := io.Copy(f, r); err != nil {
		t.Fatalf("copy: %v", err)
	}
	f.Close()

	// Index it into a fresh bare.
	dst := filepath.Join(dir, "dst.git")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := InitBare(ctx, dst); err != nil {
		t.Fatalf("InitBare: %v", err)
	}
	idxPath, err := IndexPackStrict(ctx, dst, tmp)
	if err != nil {
		t.Fatalf("IndexPackStrict: %v", err)
	}
	if _, err := os.Stat(idxPath); err != nil {
		t.Fatalf("idx not present: %v", err)
	}
}

func TestIndexPackStrict_RejectsCorruptPack(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	dir := t.TempDir()
	dst := filepath.Join(dir, "dst.git")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := InitBare(context.Background(), dst); err != nil {
		t.Fatalf("InitBare: %v", err)
	}
	bad := filepath.Join(dir, "bad.pack")
	if err := os.WriteFile(bad, []byte("PACK\x00\x00\x00\x02not a real pack"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := IndexPackStrict(context.Background(), dst, bad); err == nil {
		t.Fatalf("IndexPackStrict: expected error on corrupt pack")
	}
}

func TestRevListNotAll_EmptyMeansClean(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	dir := t.TempDir()
	bare := filepath.Join(dir, "src.git")
	mustGit(t, dir, "init", "--bare", bare)
	work := filepath.Join(dir, "wt")
	mustGit(t, dir, "clone", bare, work)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustGit(t, work, "add", ".")
	mustGit(t, work, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "x")
	mustGit(t, work, "push", "origin", "HEAD:refs/heads/main")
	tip := strings.TrimSpace(string(mustGitCapture(t, work, "rev-parse", "HEAD")))

	missing, err := RevListNotAll(context.Background(), bare, []string{tip})
	if err != nil {
		t.Fatalf("RevListNotAll: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("RevListNotAll: expected empty, got %v", missing)
	}
}

func TestFsckConnectivityOnly_PassesOnCleanRepo(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	dir := t.TempDir()
	bare := filepath.Join(dir, "src.git")
	mustGit(t, dir, "init", "--bare", bare)
	work := filepath.Join(dir, "wt")
	mustGit(t, dir, "clone", bare, work)
	_ = os.WriteFile(filepath.Join(work, "a.txt"), []byte("x"), 0o644)
	mustGit(t, work, "add", ".")
	mustGit(t, work, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "x")
	mustGit(t, work, "push", "origin", "HEAD:refs/heads/main")

	if err := FsckConnectivityOnly(context.Background(), bare); err != nil {
		t.Fatalf("FsckConnectivityOnly: %v", err)
	}
}

func TestUpdateRefDelete_Removes(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	dir := t.TempDir()
	bare := filepath.Join(dir, "src.git")
	mustGit(t, dir, "init", "--bare", bare)
	work := filepath.Join(dir, "wt")
	mustGit(t, dir, "clone", bare, work)
	_ = os.WriteFile(filepath.Join(work, "a.txt"), []byte("x"), 0o644)
	mustGit(t, work, "add", ".")
	mustGit(t, work, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "x")
	mustGit(t, work, "push", "origin", "HEAD:refs/heads/temp")
	oid := strings.TrimSpace(string(mustGitCapture(t, work, "rev-parse", "HEAD")))

	if err := UpdateRefDelete(context.Background(), bare, "refs/heads/temp", oid); err != nil {
		t.Fatalf("UpdateRefDelete: %v", err)
	}
	out, err := RunForTest(bare, "rev-parse", "--verify", "refs/heads/temp")
	if err == nil {
		t.Fatalf("ref not deleted: %s", out)
	}
}

func TestUpdateRefDelete_RejectsBadInput(t *testing.T) {
	ctx := context.Background()
	if err := UpdateRefDelete(ctx, t.TempDir(), "bad ref", "1111111111111111111111111111111111111111"); err == nil {
		t.Fatalf("expected error on bad ref name")
	}
	if err := UpdateRefDelete(ctx, t.TempDir(), "refs/heads/main", "not-an-oid"); err == nil {
		t.Fatalf("expected error on bad oid")
	}
}

func TestUpdateRefCAS_HappyPath(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	dir := t.TempDir()
	bare := filepath.Join(dir, "src.git")
	mustGit(t, dir, "init", "--bare", bare)
	work := filepath.Join(dir, "wt")
	mustGit(t, dir, "clone", bare, work)
	_ = os.WriteFile(filepath.Join(work, "a.txt"), []byte("v1"), 0o644)
	mustGit(t, work, "add", ".")
	mustGit(t, work, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "v1")
	mustGit(t, work, "push", "origin", "HEAD:refs/heads/main")
	oldOID := strings.TrimSpace(string(mustGitCapture(t, bare, "rev-parse", "refs/heads/main")))
	_ = os.WriteFile(filepath.Join(work, "a.txt"), []byte("v2"), 0o644)
	mustGit(t, work, "add", ".")
	mustGit(t, work, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "v2")
	newOID := strings.TrimSpace(string(mustGitCapture(t, work, "rev-parse", "HEAD")))
	mustGit(t, work, "push", "origin", "HEAD:refs/heads/main")

	// Reset bare's main back to oldOID so the CAS has work to do.
	mustGit(t, bare, "update-ref", "refs/heads/main", oldOID)
	if err := UpdateRefCAS(context.Background(), bare, "refs/heads/main", newOID, oldOID); err != nil {
		t.Fatalf("UpdateRefCAS: %v", err)
	}
	got := strings.TrimSpace(string(mustGitCapture(t, bare, "rev-parse", "refs/heads/main")))
	if got != newOID {
		t.Fatalf("ref not updated: %s vs %s", got, newOID)
	}
}

func TestUpdateRefCAS_RejectsStaleOldOID(t *testing.T) {
	if testing.Short() {
		t.Skip("requires git binary")
	}
	dir := t.TempDir()
	bare := filepath.Join(dir, "src.git")
	mustGit(t, dir, "init", "--bare", bare)
	work := filepath.Join(dir, "wt")
	mustGit(t, dir, "clone", bare, work)
	_ = os.WriteFile(filepath.Join(work, "a.txt"), []byte("v1"), 0o644)
	mustGit(t, work, "add", ".")
	mustGit(t, work, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "v1")
	mustGit(t, work, "push", "origin", "HEAD:refs/heads/main")
	currentOID := strings.TrimSpace(string(mustGitCapture(t, bare, "rev-parse", "refs/heads/main")))

	// Try CAS with a wrong oldOID — should fail.
	wrongOldOID := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	someNewOID := currentOID // doesn't matter what we set, the CAS check fails first
	if err := UpdateRefCAS(context.Background(), bare, "refs/heads/main", someNewOID, wrongOldOID); err == nil {
		t.Fatalf("UpdateRefCAS: expected error on stale oldOID")
	}
}

// TestRevParse_Branch verifies that RevParse against a bare repo
// resolves refs/heads/<name> to the same 40-hex OID that the underlying
// git binary reports via `git rev-parse`. The fixture builds a single-
// commit bare mirror and compares; bundle generation (M11 Phase 3) calls
// RevParse to capture the tip OID stored on BundleEntry.TipOID, so this
// happy-path test guards that contract.
func TestRevParse_Branch(t *testing.T) {
	skipIfNoGit(t)
	bare := makeRepoWithOneCommit(t)
	refs, err := ShowRef(context.Background(), bare)
	if err != nil {
		t.Fatalf("ShowRef: %v", err)
	}
	// Pick a deterministic ref — map iteration order is randomized and
	// the fixture may grow to multiple refs in the future.
	const ref = "refs/heads/main"
	want, ok := refs[ref]
	if !ok {
		t.Fatalf("expected fixture to contain %q; got refs=%v", ref, refs)
	}
	got, err := RevParse(context.Background(), bare, ref)
	if err != nil {
		t.Fatalf("RevParse(%q): %v", ref, err)
	}
	if got != want {
		t.Fatalf("RevParse(%q) = %q, want %q", ref, got, want)
	}
}

// TestRevParse_RejectsBadRef confirms that validRefOrOID is the gate:
// dash-prefixed refs are refused before git is invoked.
func TestRevParse_RejectsBadRef(t *testing.T) {
	skipIfNoGit(t)
	dir := t.TempDir()
	if err := InitBare(context.Background(), dir); err != nil {
		t.Fatalf("InitBare: %v", err)
	}
	if _, err := RevParse(context.Background(), dir, "--config=foo"); err == nil {
		t.Fatalf("expected rejection of dash-prefixed ref")
	}
}

func TestRevListNotAll_RejectsNonHexInputs(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	bare := filepath.Join(dir, "bare.git")
	mustGit(t, dir, "init", "--bare", bare)
	for _, bad := range []string{
		"HEAD",
		"refs/heads/main",
		"main",
		"deadbeef", // short
		"HEAD^",
		"main..feature",
		"DEADBEEFDEADBEEFDEADBEEFDEADBEEFDEADBEEF", // uppercase
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbee",  // 39 chars
		"-evil",
		"\nevil",
	} {
		if _, err := RevListNotAll(ctx, bare, []string{bad}); err == nil {
			t.Fatalf("RevListNotAll(%q): expected error", bad)
		}
	}
}
