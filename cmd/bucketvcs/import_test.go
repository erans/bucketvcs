package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

// makeBareForTest authors a tiny bare git repo for CLI tests.
func makeBareForTest(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	mustGit := func(args ...string) {
		t.Helper()
		if out, err := gitcli.RunForTest(work, args...); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	mustGit("init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(work, "f"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mustGit("add", "f")
	mustGit("-c", "user.name=t", "-c", "user.email=t@e", "commit", "-m", "init")
	bare := filepath.Join(t.TempDir(), "bare")
	if err := gitcli.CloneBareMirror(context.Background(), work, bare); err != nil {
		t.Fatalf("CloneBareMirror: %v", err)
	}
	return bare
}

func TestImportCmd_HappyPath(t *testing.T) {
	if _, err := gitcli.Version(context.Background()); err != nil {
		t.Skip("git not available")
	}
	src := makeBareForTest(t)
	storeRoot := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(),
		[]string{"import", "--store=localfs:" + storeRoot, src, "acme", "x"},
		&stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	// Verify progress lines on stderr.
	for _, want := range []string{"fsck source ok", "pack built ", "uploaded pack", "uploaded indexes", "commit "} {
		if !bytes.Contains(stderr.Bytes(), []byte(want)) {
			t.Fatalf("stderr missing %q: %q", want, stderr.String())
		}
	}
}

func TestImportCmd_RejectsExistingRepo(t *testing.T) {
	if _, err := gitcli.Version(context.Background()); err != nil {
		t.Skip("git not available")
	}
	src := makeBareForTest(t)
	storeRoot := t.TempDir()
	var sink bytes.Buffer
	if code := run(context.Background(),
		[]string{"import", "--store=localfs:" + storeRoot, src, "t", "r"},
		&sink, &sink); code != 0 {
		t.Fatalf("first import exit=%d", code)
	}
	sink.Reset()
	code := run(context.Background(),
		[]string{"import", "--store=localfs:" + storeRoot, src, "t", "r"},
		&sink, &sink)
	if code != 2 {
		t.Fatalf("second import exit=%d, want 2", code)
	}
}

func TestImportCmd_MissingStore(t *testing.T) {
	var sink bytes.Buffer
	code := run(context.Background(),
		[]string{"import", "src", "t", "r"},
		&sink, &sink)
	if code != 2 {
		t.Fatalf("missing --store exit=%d, want 2", code)
	}
}
