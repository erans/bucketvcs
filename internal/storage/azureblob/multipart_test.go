package azureblob

import "testing"

func TestMakeBlockIDFixedLength(t *testing.T) {
	id, _ := newUploadID()
	a := makeBlockID(id, 1)
	b := makeBlockID(id, 12345)
	if len(a) != len(b) {
		t.Fatalf("block IDs must be equal length within an upload: len(a)=%d len(b)=%d", len(a), len(b))
	}
}

func TestMakeBlockIDUploadIsolated(t *testing.T) {
	idA, _ := newUploadID()
	idB, _ := newUploadID()
	a := makeBlockID(idA, 1)
	b := makeBlockID(idB, 1)
	if a == b {
		t.Fatalf("block IDs from different uploads must differ even at same partNumber")
	}
}
