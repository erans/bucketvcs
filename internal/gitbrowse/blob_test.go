package gitbrowse

import (
	"context"
	"testing"
)

func TestReadBlob_Text(t *testing.T) {
	svc, tenant, repo, oids := fixture(t)
	b, err := svc.ReadBlob(context.Background(), tenant, repo, oids["c1"], "a.txt")
	if err != nil {
		t.Fatalf("ReadBlob: %v", err)
	}
	if b.Binary || b.TooLarge || string(b.Bytes) != "hello\n" || b.Size != 6 {
		t.Fatalf("got %+v", b)
	}
}

func TestReadBlob_Binary(t *testing.T) {
	svc, tenant, repo, oids := fixture(t)
	b, err := svc.ReadBlob(context.Background(), tenant, repo, oids["c2"], "bin.dat")
	if err != nil {
		t.Fatalf("ReadBlob: %v", err)
	}
	// Binary blobs are flagged Binary=true but STILL carry their bytes (so the
	// raw endpoint can serve them); only TooLarge yields nil Bytes.
	if !b.Binary {
		t.Fatalf("want Binary=true, got %+v", b)
	}
	if len(b.Bytes) != 4 || b.Size != 4 {
		t.Fatalf("binary blob should carry its 4 bytes, got %+v", b)
	}
}

func TestReadBlob_Missing(t *testing.T) {
	svc, tenant, repo, oids := fixture(t)
	if _, err := svc.ReadBlob(context.Background(), tenant, repo, oids["c2"], "nope.txt"); err == nil {
		t.Fatal("expected error for missing blob")
	}
}
