package authreplica

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/superfly/ltx"

	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func newLocalFS(t *testing.T) storage.ObjectStore {
	t.Helper()
	s, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// ltxPayload returns deterministic filler bytes. They are NOT a valid LTX
// file: Client tolerates that by falling back to store timestamps when
// PeekHeader fails (see WriteLTXFile). Real-LTX round-trips are covered by
// the Runner integration test added in a later task.
func ltxPayload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

func TestClient_WriteOpenRoundTrip(t *testing.T) {
	ctx := context.Background()
	c := NewClient(newLocalFS(t), "sys/authdb")
	body := ltxPayload(4096)

	info, err := c.WriteLTXFile(ctx, 0, ltx.TXID(1), ltx.TXID(5), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if info.Level != 0 || info.MinTXID != 1 || info.MaxTXID != 5 || info.Size != int64(len(body)) {
		t.Fatalf("bad FileInfo: %+v", info)
	}

	rc, err := c.OpenLTXFile(ctx, 0, ltx.TXID(1), ltx.TXID(5), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, body) {
		t.Fatalf("round-trip mismatch: got %d bytes", len(got))
	}
}

func TestClient_OpenLTXFile_Range(t *testing.T) {
	ctx := context.Background()
	c := NewClient(newLocalFS(t), "sys/authdb")
	body := ltxPayload(1000)
	if _, err := c.WriteLTXFile(ctx, 1, 10, 20, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}

	// offset+size
	rc, err := c.OpenLTXFile(ctx, 1, 10, 20, 100, 50)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, body[100:150]) {
		t.Fatalf("range mismatch: got %d bytes", len(got))
	}

	// offset, size=0 → rest of file
	rc, err = c.OpenLTXFile(ctx, 1, 10, 20, 900, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, body[900:]) {
		t.Fatalf("tail range mismatch: got %d bytes", len(got))
	}

	// offset at EOF with size=0 → empty reader, no error (reference behavior)
	rc, err = c.OpenLTXFile(ctx, 1, 10, 20, 1000, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err = io.ReadAll(rc)
	rc.Close()
	if err != nil || len(got) != 0 {
		t.Fatalf("EOF tail: want empty read, got %d bytes err=%v", len(got), err)
	}
}

func TestClient_OpenLTXFile_NotExist(t *testing.T) {
	c := NewClient(newLocalFS(t), "sys/authdb")
	_, err := c.OpenLTXFile(context.Background(), 0, 1, 2, 0, 0)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("want os.ErrNotExist, got %v", err)
	}
}

func TestClient_LTXFiles_OrderAndSeek(t *testing.T) {
	ctx := context.Background()
	c := NewClient(newLocalFS(t), "sys/authdb")
	for _, r := range [][2]ltx.TXID{{1, 5}, {6, 9}, {10, 12}} {
		if _, err := c.WriteLTXFile(ctx, 0, r[0], r[1], bytes.NewReader(ltxPayload(64))); err != nil {
			t.Fatal(err)
		}
	}

	itr, err := c.LTXFiles(ctx, 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	var mins []ltx.TXID
	for itr.Next() {
		mins = append(mins, itr.Item().MinTXID)
	}
	itr.Close()
	if len(mins) != 3 || mins[0] != 1 || mins[1] != 6 || mins[2] != 10 {
		t.Fatalf("bad order: %v", mins)
	}

	// seek skips files with MinTXID < seek
	itr, err = c.LTXFiles(ctx, 0, 6, false)
	if err != nil {
		t.Fatal(err)
	}
	mins = nil
	for itr.Next() {
		mins = append(mins, itr.Item().MinTXID)
	}
	itr.Close()
	if len(mins) != 2 || mins[0] != 6 {
		t.Fatalf("bad seek result: %v", mins)
	}

	// empty level → empty iterator, no error
	itr, err = c.LTXFiles(ctx, 7, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if itr.Next() {
		t.Fatal("expected empty iterator")
	}
	itr.Close()
}

func TestClient_WriteLTXFile_OverwritesExisting(t *testing.T) {
	// Crash-retry semantics: litestream may rewrite the same (level,min,max).
	// Native clients do an unconditional PUT; we synthesize it from CAS.
	ctx := context.Background()
	c := NewClient(newLocalFS(t), "sys/authdb")
	if _, err := c.WriteLTXFile(ctx, 0, 1, 5, bytes.NewReader(ltxPayload(64))); err != nil {
		t.Fatal(err)
	}
	second := ltxPayload(128)
	if _, err := c.WriteLTXFile(ctx, 0, 1, 5, bytes.NewReader(second)); err != nil {
		t.Fatal(err)
	}
	rc, err := c.OpenLTXFile(ctx, 0, 1, 5, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, second) {
		t.Fatal("second write did not win")
	}
}

func TestClient_DeleteLTXFiles_Idempotent(t *testing.T) {
	ctx := context.Background()
	c := NewClient(newLocalFS(t), "sys/authdb")
	if _, err := c.WriteLTXFile(ctx, 0, 1, 5, bytes.NewReader(ltxPayload(64))); err != nil {
		t.Fatal(err)
	}
	infos := []*ltx.FileInfo{
		{Level: 0, MinTXID: 1, MaxTXID: 5},
		{Level: 0, MinTXID: 90, MaxTXID: 99}, // never existed — must not error
	}
	if err := c.DeleteLTXFiles(ctx, infos); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteLTXFiles(ctx, infos); err != nil { // repeat — idempotent
		t.Fatal(err)
	}
	if _, err := c.OpenLTXFile(ctx, 0, 1, 5, 0, 0); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("want gone, got %v", err)
	}
}

func TestClient_DeleteAll(t *testing.T) {
	ctx := context.Background()
	c := NewClient(newLocalFS(t), "sys/authdb")
	for lvl := 0; lvl < 3; lvl++ {
		if _, err := c.WriteLTXFile(ctx, lvl, 1, 5, bytes.NewReader(ltxPayload(64))); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.DeleteAll(ctx); err != nil {
		t.Fatal(err)
	}
	itr, err := c.LTXFiles(ctx, 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if itr.Next() {
		t.Fatal("expected no files after DeleteAll")
	}
	itr.Close()
}
