package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

func TestExportCmd_HappyPath(t *testing.T) {
	if _, err := gitcli.Version(context.Background()); err != nil {
		t.Skip("git not available")
	}
	src := makeBareForTest(t)
	storeRoot := t.TempDir()
	var sink bytes.Buffer
	if code := run(context.Background(),
		[]string{"import", "--store=localfs:" + storeRoot, src, "t", "r"},
		&sink, &sink); code != 0 {
		t.Fatalf("import: exit=%d", code)
	}
	dst := filepath.Join(t.TempDir(), "out")
	sink.Reset()
	if code := run(context.Background(),
		[]string{"export", "--store=localfs:" + storeRoot, "t", "r", dst},
		&sink, &sink); code != 0 {
		t.Fatalf("export: exit=%d stderr=%q", code, sink.String())
	}
	if _, err := os.Stat(filepath.Join(dst, "objects")); err != nil {
		t.Fatalf("expected objects/: %v", err)
	}
}

func TestExportCmd_NotFound(t *testing.T) {
	storeRoot := t.TempDir()
	dst := filepath.Join(t.TempDir(), "out")
	var sink bytes.Buffer
	code := run(context.Background(),
		[]string{"export", "--store=localfs:" + storeRoot, "absent", "absent", dst},
		&sink, &sink)
	if code != 2 {
		t.Fatalf("exit=%d, want 2", code)
	}
}
