package maintenance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/maintenance/mtest"
)

func TestBuildIndexesFromLocalPack_HashesAreContentAddressed(t *testing.T) {
	rp := mtest.SetupRepackedPack(t)
	res, err := buildIndexesFromLocalPack(context.Background(),
		rp.PackPath, rp.IdxPath, rp.PackID, rp.Refs)
	if err != nil {
		t.Fatalf("buildIndexesFromLocalPack: %v", err)
	}
	if len(res.ObjectMapBytes) == 0 {
		t.Fatalf("ObjectMapBytes empty")
	}
	bvomSum := sha256.Sum256(res.ObjectMapBytes)
	if res.ObjectMapHash != hex.EncodeToString(bvomSum[:]) {
		t.Errorf("ObjectMapHash != sha256(bytes)")
	}
	if len(res.CommitGraphBytes) == 0 {
		t.Fatalf("CommitGraphBytes empty")
	}
	bvcgSum := sha256.Sum256(res.CommitGraphBytes)
	if res.CommitGraphHash != hex.EncodeToString(bvcgSum[:]) {
		t.Errorf("CommitGraphHash != sha256(bytes)")
	}
	if res.ObjectCount <= 0 {
		t.Errorf("ObjectCount = %d", res.ObjectCount)
	}
	if res.PackSizeBytes <= 0 {
		t.Errorf("PackSizeBytes = %d", res.PackSizeBytes)
	}
}
