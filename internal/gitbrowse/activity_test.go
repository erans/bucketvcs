package gitbrowse

import (
	"context"
	"testing"
)

func TestTreeActivity_RootAttribution(t *testing.T) {
	svc, tenant, repo, oids := fixture(t)
	act, err := svc.TreeActivity(context.Background(), tenant, repo, oids["c2"], "")
	if err != nil {
		t.Fatalf("TreeActivity: %v", err)
	}
	a, ok := act["a.txt"]
	if !ok || a.Summary != "update a" || a.OID != oids["c2"] {
		t.Fatalf("a.txt attribution = %+v (ok=%v)", a, ok)
	}
	sub, ok := act["sub"]
	if !ok || sub.Summary != "init" {
		t.Fatalf("sub attribution = %+v (ok=%v)", sub, ok)
	}
}

func TestTreeActivity_ScopedToSubdir(t *testing.T) {
	svc, tenant, repo, oids := fixture(t)
	act, err := svc.TreeActivity(context.Background(), tenant, repo, oids["c2"], "sub")
	if err != nil {
		t.Fatalf("TreeActivity sub: %v", err)
	}
	if m, ok := act["sub/b.txt"]; !ok || m.Summary != "init" {
		t.Fatalf("sub/b.txt attribution = %+v (ok=%v)", m, ok)
	}
	if _, ok := act["a.txt"]; ok {
		t.Fatal("root file leaked into scoped activity")
	}
}

func TestParseNameStatusWalk(t *testing.T) {
	raw := "\x1e" + "f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1\x1fAnn\x1fann@x\x1f1700000000\x1fsecond\n" +
		"M\tdir/x.txt\n" +
		"\x1e" + "e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0\x1fAnn\x1fann@x\x1f1699999999\x1ffirst\n" +
		"A\ta.txt\nA\tdir/x.txt\n"
	recs := parseNameStatusWalk([]byte(raw))
	if len(recs) != 2 {
		t.Fatalf("want 2 records, got %d", len(recs))
	}
	if recs[0].meta.Summary != "second" || len(recs[0].paths) != 1 || recs[0].paths[0] != "dir/x.txt" {
		t.Fatalf("rec0 = %+v", recs[0])
	}
	if recs[1].meta.AuthorTime != 1699999999 || len(recs[1].paths) != 2 {
		t.Fatalf("rec1 = %+v", recs[1])
	}
}
