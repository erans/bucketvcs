package gitbrowse

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/browsemodel"
)

func TestOpenMirror_ReturnsBareDir(t *testing.T) {
	svc, tenant, repo, _ := fixture(t)
	m, release, err := svc.openMirror(context.Background(), tenant, repo)
	if err != nil {
		t.Fatalf("openMirror: %v", err)
	}
	defer release()
	if m.BareDir() == "" {
		t.Fatal("empty bare dir")
	}
}

func TestOpenMirror_TimeoutIsWarming(t *testing.T) {
	svc, tenant, repo, _ := fixture(t)
	svc.timeout = 1 * time.Nanosecond // force the cold-open deadline to blow
	_, _, err := svc.openMirror(context.Background(), tenant, repo)
	if !errors.Is(err, browsemodel.ErrWarming) {
		t.Fatalf("want ErrWarming, got %v", err)
	}
}

func TestListRefs(t *testing.T) {
	svc, tenant, repo, _ := fixture(t)
	refs, err := svc.ListRefs(context.Background(), tenant, repo)
	if err != nil {
		t.Fatalf("ListRefs: %v", err)
	}
	if refs.Default != "main" {
		t.Fatalf("default = %q, want main", refs.Default)
	}
	names := map[string]bool{}
	for _, b := range refs.Branches {
		names[b.Name] = true
		if len(b.OID) != 40 {
			t.Fatalf("branch %q bad oid %q", b.Name, b.OID)
		}
	}
	if !names["main"] || !names["feature/foo"] {
		t.Fatalf("missing branches: %+v", refs.Branches)
	}
	tagNames := map[string]bool{}
	for _, tg := range refs.Tags {
		tagNames[tg.Name] = true
	}
	if !tagNames["v1.0"] {
		t.Fatalf("missing tag v1.0: %+v", refs.Tags)
	}
}

func TestResolve_SlashRefVsPath(t *testing.T) {
	svc, tenant, repo, oids := fixture(t)
	ctx := context.Background()

	// "feature/foo/c.txt" must split ref="feature/foo", path="c.txt".
	r, err := svc.Resolve(ctx, tenant, repo, "feature/foo/c.txt")
	if err != nil {
		t.Fatalf("Resolve slash ref: %v", err)
	}
	if r.Ref != "feature/foo" || r.Path != "c.txt" || r.OID != oids["feat"] {
		t.Fatalf("got %+v", r)
	}

	// "main" alone resolves to a ref with empty path.
	r, err = svc.Resolve(ctx, tenant, repo, "main")
	if err != nil || r.Ref != "main" || r.Path != "" || r.OID != oids["c2"] {
		t.Fatalf("Resolve main: %v %+v", err, r)
	}

	// A raw 40-hex OID resolves with empty ref.
	r, err = svc.Resolve(ctx, tenant, repo, oids["c1"]+"/a.txt")
	if err != nil || r.Ref != "" || r.OID != oids["c1"] || r.Path != "a.txt" {
		t.Fatalf("Resolve oid: %v %+v", err, r)
	}

	// Unknown ref → ErrNotFound.
	if _, err := svc.Resolve(ctx, tenant, repo, "nope/x.txt"); !errors.Is(err, browsemodel.ErrNotFound) {
		t.Fatalf("want ErrNotFound for unknown ref, got %v", err)
	}
}
