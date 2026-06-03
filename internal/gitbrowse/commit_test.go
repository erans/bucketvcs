package gitbrowse

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestParseCommitObject(t *testing.T) {
	raw := "tree 1111111111111111111111111111111111111111\n" +
		"parent 2222222222222222222222222222222222222222\n" +
		"author Ann <ann@x> 1700000000 +0000\n" +
		"committer Ann <ann@x> 1700000000 +0000\n" +
		"\n" +
		"update a\n\nbody line\n"
	meta, parents, msg, err := parseCommitObject([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if meta.AuthorName != "Ann" || meta.AuthorEmail != "ann@x" || meta.AuthorTime != 1700000000 {
		t.Fatalf("meta = %+v", meta)
	}
	if meta.Summary != "update a" {
		t.Fatalf("summary = %q", meta.Summary)
	}
	if len(parents) != 1 || parents[0] != "2222222222222222222222222222222222222222" {
		t.Fatalf("parents = %v", parents)
	}
	if msg != "update a\n\nbody line\n" {
		t.Fatalf("msg = %q", msg)
	}
}

func TestParseUnifiedDiff(t *testing.T) {
	patch := "diff --git a/a.txt b/a.txt\n" +
		"index e0e..f1f 100644\n" +
		"--- a/a.txt\n" +
		"+++ b/a.txt\n" +
		"@@ -1 +1 @@\n" +
		"-hello\n" +
		"+hello again\n" +
		"diff --git a/bin.dat b/bin.dat\n" +
		"new file mode 100644\n" +
		"index 000..abc\n" +
		"Binary files /dev/null and b/bin.dat differ\n"
	files, truncated := parseUnifiedDiff([]byte(patch))
	if truncated {
		t.Fatal("unexpected truncation")
	}
	if len(files) != 2 {
		t.Fatalf("want 2 files, got %d", len(files))
	}
	if files[0].NewPath != "a.txt" || files[0].Additions != 1 || files[0].Deletions != 1 {
		t.Fatalf("file0 = %+v", files[0])
	}
	if len(files[0].Hunks) != 1 || files[0].Hunks[0].Header != "@@ -1 +1 @@" {
		t.Fatalf("file0 hunks = %+v", files[0].Hunks)
	}
	if !files[1].Binary || files[1].NewPath != "bin.dat" {
		t.Fatalf("file1 = %+v", files[1])
	}
}

func TestCommit_EndToEnd(t *testing.T) {
	svc, tenant, repo, oids := fixture(t)
	cd, err := svc.Commit(context.Background(), tenant, repo, oids["c2"])
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if cd.Meta.OID != oids["c2"] || cd.Meta.Summary != "update a" {
		t.Fatalf("meta = %+v", cd.Meta)
	}
	if len(cd.Parents) != 1 || cd.Parents[0] != oids["c1"] {
		t.Fatalf("parents = %v", cd.Parents)
	}
	var sawA bool
	for _, f := range cd.Files {
		if f.NewPath == "a.txt" {
			sawA = true
		}
	}
	if !sawA {
		t.Fatalf("expected a.txt in diff, files = %+v", cd.Files)
	}
}

func TestParseUnifiedDiff_PerFileLineCap(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("diff --git a/big.txt b/big.txt\n--- a/big.txt\n+++ b/big.txt\n@@ -0,0 +1,4000 @@\n")
	for i := 0; i < 4000; i++ {
		sb.WriteString("+x\n")
	}
	files, truncated := parseUnifiedDiff([]byte(sb.String()))
	if truncated {
		t.Fatal("commit-level truncation should not trip for one file")
	}
	if len(files) != 1 || !files[0].TooLarge {
		t.Fatalf("want 1 TooLarge file, got %+v", files)
	}
	if files[0].Hunks != nil {
		t.Fatalf("TooLarge file must have no hunks, got %d", len(files[0].Hunks))
	}
	if files[0].Additions != 4000 {
		t.Fatalf("Additions = %d, want true total 4000", files[0].Additions)
	}
}

func TestParseUnifiedDiff_FileCountCap(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 305; i++ {
		fmt.Fprintf(&sb, "diff --git a/f%d.txt b/f%d.txt\n--- a/f%d.txt\n+++ b/f%d.txt\n@@ -1 +1 @@\n-a\n+b\n", i, i, i, i)
	}
	files, truncated := parseUnifiedDiff([]byte(sb.String()))
	if !truncated {
		t.Fatal("expected commit-level truncation at 300 files")
	}
	if len(files) != 300 {
		t.Fatalf("len(files) = %d, want 300", len(files))
	}
}

func TestParseUnifiedDiff_DashDashContentLines(t *testing.T) {
	patch := "diff --git a/sig.txt b/sig.txt\n" +
		"--- a/sig.txt\n" +
		"+++ b/sig.txt\n" +
		"@@ -1,2 +1,2 @@\n" +
		" context\n" +
		"--- removed signature\n" + // removed line whose content is "-- removed signature"
		"+++ added rule\n" + // added line whose content is "++ added rule"
		"@@ -10 +10 @@\n" +
		"-x\n" +
		"+y\n"
	files, _ := parseUnifiedDiff([]byte(patch))
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %d", len(files))
	}
	f := files[0]
	if f.OldPath != "sig.txt" || f.NewPath != "sig.txt" {
		t.Fatalf("paths corrupted by content lines: %+v", f)
	}
	if f.Additions != 2 || f.Deletions != 2 {
		t.Fatalf("counts = +%d -%d, want +2 -2", f.Additions, f.Deletions)
	}
	if len(f.Hunks) != 2 {
		t.Fatalf("want 2 hunks, got %d", len(f.Hunks))
	}
	if got := f.Hunks[0].Lines[1]; got.Kind != '-' || got.Text != "-- removed signature" {
		t.Fatalf("mid-hunk dash line misparsed: %+v", got)
	}
}

func TestCommit_MergeShowsFirstParentDiff(t *testing.T) {
	svc, tenant, repo, oids := fixture(t)
	cd, err := svc.Commit(context.Background(), tenant, repo, oids["merge"])
	if err != nil {
		t.Fatalf("Commit(merge): %v", err)
	}
	if len(cd.Parents) != 2 {
		t.Fatalf("parents = %v, want 2 (merge)", cd.Parents)
	}
	// Diff vs first parent (merged base) must show the branch's file arriving.
	var sawC bool
	for _, f := range cd.Files {
		if f.NewPath == "c.txt" {
			sawC = true
		}
	}
	if !sawC {
		t.Fatalf("merge diff missing c.txt (first-parent diff suppressed?), files = %+v", cd.Files)
	}
}

func TestCommit_NonASCIIPathUnquoted(t *testing.T) {
	svc, tenant, repo, oids := fixture(t)
	cd, err := svc.Commit(context.Background(), tenant, repo, oids["c2"])
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	var saw bool
	for _, f := range cd.Files {
		if f.NewPath == "café.txt" {
			saw = true
		}
		if strings.Contains(f.NewPath, "\\303") || strings.HasPrefix(f.NewPath, "\"") {
			t.Fatalf("c-quoted path leaked: %q", f.NewPath)
		}
	}
	if !saw {
		t.Fatalf("café.txt not in c2 diff: %+v", cd.Files)
	}
}
