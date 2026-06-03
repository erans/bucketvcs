package gitbrowse

import (
	"context"
	"testing"
)

func TestParseLog(t *testing.T) {
	raw := "f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1\x1fAnn\x1fann@x\x1f1700000000\x1fsecond\x1e" +
		"e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0e0\x1fAnn\x1fann@x\x1f1699999999\x1ffirst\x1e"
	metas, err := parseLog([]byte(raw))
	if err != nil {
		t.Fatalf("parseLog: %v", err)
	}
	if len(metas) != 2 {
		t.Fatalf("want 2, got %d", len(metas))
	}
	if metas[0].Summary != "second" || metas[0].AuthorName != "Ann" || metas[0].AuthorTime != 1700000000 {
		t.Fatalf("meta0 = %+v", metas[0])
	}
	if metas[0].ShortOID != "f1f1f1f1f1f1" {
		t.Fatalf("shortoid = %q", metas[0].ShortOID)
	}
}

func TestLog_Pagination(t *testing.T) {
	svc, tenant, repo, oids := fixture(t)
	ctx := context.Background()
	// main has 2 commits; page size 1 → first page has more=true.
	page, more, err := svc.Log(ctx, tenant, repo, oids["c2"], 0, 1)
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	if len(page) != 1 || !more {
		t.Fatalf("page0: len=%d more=%v", len(page), more)
	}
	page2, more2, err := svc.Log(ctx, tenant, repo, oids["c2"], 1, 1)
	if err != nil {
		t.Fatalf("Log p2: %v", err)
	}
	if len(page2) != 1 || more2 {
		t.Fatalf("page1: len=%d more=%v", len(page2), more2)
	}
}
