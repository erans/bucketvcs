package gitbrowse

import (
	"context"
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
