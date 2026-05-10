package maintenance

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalFilePackStore_GetReturnsFileBytes(t *testing.T) {
	tmp := t.TempDir()
	packPath := filepath.Join(tmp, "p.pack")
	idxPath := filepath.Join(tmp, "p.idx")
	if err := os.WriteFile(packPath, []byte("PACK-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idxPath, []byte("IDX-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := newLocalFilePackStore(packPath, idxPath)
	if err != nil {
		t.Fatalf("newLocalFilePackStore: %v", err)
	}
	ctx := context.Background()

	for _, tc := range []struct {
		key, want string
	}{
		{"p.pack", "PACK-bytes"},
		{"p.idx", "IDX-bytes"},
	} {
		obj, err := s.Get(ctx, tc.key, nil)
		if err != nil {
			t.Fatalf("Get(%s): %v", tc.key, err)
		}
		body, err := io.ReadAll(obj.Body)
		obj.Body.Close()
		if err != nil {
			t.Fatalf("ReadAll(%s): %v", tc.key, err)
		}
		if string(body) != tc.want {
			t.Errorf("Get(%s) = %q, want %q", tc.key, body, tc.want)
		}
		if int(obj.Metadata.Size) != len(tc.want) {
			t.Errorf("Get(%s).Metadata.Size = %d, want %d", tc.key, obj.Metadata.Size, len(tc.want))
		}
	}
}

func TestLocalFilePackStore_HeadReturnsSize(t *testing.T) {
	tmp := t.TempDir()
	packPath := filepath.Join(tmp, "p.pack")
	idxPath := filepath.Join(tmp, "p.idx")
	if err := os.WriteFile(packPath, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idxPath, []byte("XX"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := newLocalFilePackStore(packPath, idxPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	md, err := s.Head(ctx, "p.pack")
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if md.Size != 10 {
		t.Errorf("Head.Size = %d, want 10", md.Size)
	}
}

func TestLocalFilePackStore_RejectsUnknownKey(t *testing.T) {
	tmp := t.TempDir()
	packPath := filepath.Join(tmp, "p.pack")
	idxPath := filepath.Join(tmp, "p.idx")
	if err := os.WriteFile(packPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idxPath, []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := newLocalFilePackStore(packPath, idxPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(context.Background(), "p.nope", nil); err == nil {
		t.Fatal("Get(unknown key) succeeded; want error")
	}
}
