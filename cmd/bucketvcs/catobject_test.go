package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

func TestCatObjectCmd_PrettyMatchesGit(t *testing.T) {
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
	refs, err := gitcli.ShowRef(context.Background(), src)
	if err != nil {
		t.Fatalf("ShowRef: %v", err)
	}
	var oid string
	for _, v := range refs {
		oid = v
		break
	}
	want, err := gitcli.CatFilePretty(context.Background(), src, oid)
	if err != nil {
		t.Fatalf("CatFilePretty: %v", err)
	}
	var stdout bytes.Buffer
	sink.Reset()
	if code := run(context.Background(),
		[]string{"cat-object", "--store=localfs:" + storeRoot, "--pretty", "t", "r", oid},
		&stdout, &sink); code != 0 {
		t.Fatalf("cat-object: exit=%d stderr=%q", code, sink.String())
	}
	if !bytes.Equal(stdout.Bytes(), want) {
		t.Fatalf("cat-object differs from git cat-file -p:\ngot=%q\nwant=%q", stdout.String(), want)
	}
}

func TestCatObjectCmd_TypeMatchesGit(t *testing.T) {
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
	refs, err := gitcli.ShowRef(context.Background(), src)
	if err != nil {
		t.Fatalf("ShowRef: %v", err)
	}
	var oid string
	for _, v := range refs {
		oid = v
		break
	}
	want, err := gitcli.CatFileType(context.Background(), src, oid)
	if err != nil {
		t.Fatalf("CatFileType: %v", err)
	}
	var stdout bytes.Buffer
	sink.Reset()
	if code := run(context.Background(),
		[]string{"cat-object", "--store=localfs:" + storeRoot, "--type", "t", "r", oid},
		&stdout, &sink); code != 0 {
		t.Fatalf("cat-object --type: exit=%d", code)
	}
	got := strings.TrimSpace(stdout.String())
	if got != want {
		t.Fatalf("type: got %q, want %q", got, want)
	}
}
