package gitcli

import (
	"bytes"
	"context"
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
		"https://ghp_TOKEN@github.com/x.git":  "https://REDACTED@github.com/x.git",
		"https://x-access-token:ghp_TOKEN@github.com/x.git": "https://REDACTED@github.com/x.git",
		// embedded in surrounding text
		"clone failed: https://u:p@h/r.git failed": "clone failed: https://REDACTED@h/r.git failed",
		"clone failed: https://TOKEN@h/r.git failed":   "clone failed: https://REDACTED@h/r.git failed",
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
	for i := 0; i < 100; i++ {
		path := filepath.Join(work, "f")
		if err := os.WriteFile(path, []byte(strings.Repeat("x", i+1)+"\n"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		mustGit("add", "f")
		mustGit("commit", "-m", "c"+strings.Repeat("x", i%10))
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
