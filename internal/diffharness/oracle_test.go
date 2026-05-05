package diffharness

import (
	"bytes"
	"context"
	"sort"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitcli"
)

func gitFsck(t *testing.T, dir string) {
	t.Helper()
	if err := gitcli.Fsck(context.Background(), dir, true); err != nil {
		t.Fatalf("fsck %s: %v", dir, err)
	}
}

func gitShowRef(t *testing.T, dir string) map[string]string {
	t.Helper()
	refs, err := gitcli.ShowRef(context.Background(), dir)
	if err != nil {
		t.Fatalf("ShowRef %s: %v", dir, err)
	}
	return refs
}

func gitRevListAllObjects(t *testing.T, dir string) []string {
	t.Helper()
	oids, err := gitcli.RevListAllObjects(context.Background(), dir)
	if err != nil {
		t.Fatalf("RevListAllObjects %s: %v", dir, err)
	}
	sort.Strings(oids)
	return oids
}

func gitCatFilePretty(t *testing.T, dir, oid string) []byte {
	t.Helper()
	out, err := gitcli.CatFilePretty(context.Background(), dir, oid)
	if err != nil {
		t.Fatalf("CatFilePretty(%s): %v", oid, err)
	}
	return out
}

func gitCatFileType(t *testing.T, dir, oid string) string {
	t.Helper()
	out, err := gitcli.CatFileType(context.Background(), dir, oid)
	if err != nil {
		t.Fatalf("CatFileType(%s): %v", oid, err)
	}
	return out
}

func gitCatFileSize(t *testing.T, dir, oid string) int64 {
	t.Helper()
	n, err := gitcli.CatFileSize(context.Background(), dir, oid)
	if err != nil {
		t.Fatalf("CatFileSize(%s): %v", oid, err)
	}
	return n
}

func equalRefs(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func equalOIDLists(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func ensureBytesEqual(t *testing.T, name string, got, want []byte) {
	t.Helper()
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs.\ngot: %q\nwant: %q", name, got, want)
	}
}
