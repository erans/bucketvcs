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

func TestListRefs_UnmaterializedRepoIsNotFound(t *testing.T) {
	svc, tenant, _, _ := fixture(t)
	// A repo with no root manifest in storage (e.g. registered --no-init,
	// never pushed) must map to ErrNotFound so the web layer 404s, not 500s.
	_, err := svc.ListRefs(context.Background(), tenant, "ghost")
	if !errors.Is(err, browsemodel.ErrNotFound) {
		t.Fatalf("want ErrNotFound for manifest-absent repo, got %v", err)
	}
}
