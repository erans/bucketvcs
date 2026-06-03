package gitbrowse

import (
	"context"
	"testing"
)

func TestParseLsTree(t *testing.T) {
	// Two NUL-terminated --long records: a blob and a tree.
	raw := "100644 blob aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa      6\ta.txt\x00" +
		"040000 tree bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb       -\tsub\x00"
	entries, err := parseLsTree([]byte(raw), "")
	if err != nil {
		t.Fatalf("parseLsTree: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}
	// Dirs sort first.
	if entries[0].Name != "sub" || entries[0].Type != "tree" {
		t.Fatalf("entry0 = %+v", entries[0])
	}
	if entries[1].Name != "a.txt" || entries[1].Type != "blob" || entries[1].Size != 6 {
		t.Fatalf("entry1 = %+v", entries[1])
	}
	if entries[1].Path != "a.txt" {
		t.Fatalf("path = %q", entries[1].Path)
	}
}

func TestReadTree_RootAndSub(t *testing.T) {
	svc, tenant, repo, oids := fixture(t)
	ctx := context.Background()
	root, err := svc.ReadTree(ctx, tenant, repo, oids["c2"], "")
	if err != nil {
		t.Fatalf("ReadTree root: %v", err)
	}
	names := map[string]string{}
	for _, e := range root {
		names[e.Name] = e.Type
	}
	if names["sub"] != "tree" || names["a.txt"] != "blob" || names["bin.dat"] != "blob" {
		t.Fatalf("root entries = %+v", root)
	}
	sub, err := svc.ReadTree(ctx, tenant, repo, oids["c2"], "sub")
	if err != nil {
		t.Fatalf("ReadTree sub: %v", err)
	}
	if len(sub) != 1 || sub[0].Name != "b.txt" || sub[0].Path != "sub/b.txt" {
		t.Fatalf("sub entries = %+v", sub)
	}
}
