package maintenance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWritePackedRefs_DeterministicSortByRefName(t *testing.T) {
	dir := t.TempDir()
	refs := map[string]string{
		"refs/heads/main":  "1111111111111111111111111111111111111111",
		"refs/heads/dev":   "2222222222222222222222222222222222222222",
		"refs/tags/v1.0.0": "3333333333333333333333333333333333333333",
	}
	if err := writePackedRefs(dir, refs); err != nil {
		t.Fatalf("writePackedRefs: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "packed-refs"))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"# pack-refs with: peeled fully-peeled sorted",
		"2222222222222222222222222222222222222222 refs/heads/dev",
		"1111111111111111111111111111111111111111 refs/heads/main",
		"3333333333333333333333333333333333333333 refs/tags/v1.0.0",
		"",
	}, "\n")
	if string(got) != want {
		t.Errorf("packed-refs:\ngot:\n%q\nwant:\n%q", got, want)
	}
}

func TestWriteHEAD_SymbolicRef(t *testing.T) {
	dir := t.TempDir()
	if err := writeHEAD(dir, "main"); err != nil {
		t.Fatalf("writeHEAD: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ref: refs/heads/main\n" {
		t.Errorf("HEAD = %q", got)
	}
}

func TestWriteHEAD_RejectsEmptyDefaultBranch(t *testing.T) {
	dir := t.TempDir()
	if err := writeHEAD(dir, ""); err == nil {
		t.Fatal("writeHEAD(empty) succeeded; want error")
	}
}

func TestWriteMinimalConfig_Bare(t *testing.T) {
	dir := t.TempDir()
	if err := writeMinimalConfig(dir); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "config"))
	if err != nil {
		t.Fatal(err)
	}
	want := "[core]\n\trepositoryformatversion = 0\n\tbare = true\n"
	if string(got) != want {
		t.Errorf("config = %q", got)
	}
}
