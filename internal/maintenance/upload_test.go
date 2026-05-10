package maintenance

import (
	"bytes"
	"context"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func TestUploadArtifacts_SuccessWritesAllFour(t *testing.T) {
	storeDir := t.TempDir()
	s, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	k, err := keys.NewRepo("acme", "site")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	in := uploadInput{
		PackID:           "abc123",
		PackBytes:        []byte("PACKBYTES"),
		IdxBytes:         []byte("IDXBYTES"),
		ObjectMapHash:    "deadbeef",
		ObjectMapBytes:   []byte("BVOM"),
		CommitGraphHash:  "cafef00d",
		CommitGraphBytes: []byte("BVCG"),
	}
	out, err := uploadArtifacts(ctx, s, k, in)
	if err != nil {
		t.Fatalf("uploadArtifacts: %v", err)
	}
	if out.PackKey == "" || out.IdxKey == "" || out.ObjectMapKey == "" || out.CommitGraphKey == "" {
		t.Fatalf("uploadResult has empty keys: %+v", out)
	}
	for _, key := range []string{out.PackKey, out.IdxKey, out.ObjectMapKey, out.CommitGraphKey} {
		if _, err := s.Head(ctx, key); err != nil {
			t.Errorf("Head(%s): %v", key, err)
		}
	}
}

// TestUploadArtifacts_PackCollisionIsBenign verifies the M9 review
// fix: pre-existing pack bytes at the canonical key are content-
// addressed by git's pack-id (SHA-1 over pack bytes), so a key
// collision implies bit-identical bytes and is treated as benign.
// upload returns success and the caller proceeds to upload the
// remaining sidecars (idx/.bvom/.bvcg).
func TestUploadArtifacts_PackCollisionIsBenign(t *testing.T) {
	storeDir := t.TempDir()
	s, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	k, err := keys.NewRepo("acme", "site")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Pre-populate the canonical pack key.
	preexistingKey := k.CanonicalPackKey("xyz")
	if _, err := s.PutIfAbsent(ctx, preexistingKey, bytes.NewReader([]byte("PACKBYTES")), nil); err != nil {
		t.Fatal(err)
	}
	in := uploadInput{
		PackID:           "xyz",
		PackBytes:        []byte("PACKBYTES"),
		IdxBytes:         []byte("IDX"),
		ObjectMapHash:    "h1",
		ObjectMapBytes:   []byte("BVOM"),
		CommitGraphHash:  "h2",
		CommitGraphBytes: []byte("BVCG"),
	}
	res, err := uploadArtifacts(ctx, s, k, in)
	if err != nil {
		t.Fatalf("uploadArtifacts (benign pack collision): %v", err)
	}
	// All four sidecars must be present in the store after upload, even
	// though the pack key collided — that's the point of the fix.
	for _, key := range []string{res.PackKey, res.IdxKey, res.ObjectMapKey, res.CommitGraphKey} {
		if _, err := s.Head(ctx, key); err != nil {
			t.Errorf("Head(%s) after benign collision: %v", key, err)
		}
	}
}

func TestUploadArtifacts_IndexAlreadyExistsIsBenign(t *testing.T) {
	storeDir := t.TempDir()
	s, err := localfs.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	k, err := keys.NewRepo("acme", "site")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Pre-populate the .bvom key with the SAME bytes we're about to upload.
	hash := "deadbeef"
	bvomKey := k.ObjectMapKey(hash)
	bvomBytes := []byte("SAMEBVOM")
	if _, err := s.PutIfAbsent(ctx, bvomKey, bytes.NewReader(bvomBytes), nil); err != nil {
		t.Fatal(err)
	}

	in := uploadInput{
		PackID:           "p1",
		PackBytes:        []byte("P"),
		IdxBytes:         []byte("I"),
		ObjectMapHash:    hash,
		ObjectMapBytes:   bvomBytes,
		CommitGraphHash:  "cafe",
		CommitGraphBytes: []byte("CG"),
	}
	if _, err := uploadArtifacts(ctx, s, k, in); err != nil {
		t.Fatalf("uploadArtifacts (benign collision): %v", err)
	}
}
