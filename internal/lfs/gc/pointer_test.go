package gc

import "testing"

func TestParsePointer_Valid(t *testing.T) {
	body := []byte("version https://git-lfs.github.com/spec/v1\n" +
		"oid sha256:1234567890123456789012345678901234567890123456789012345678901234\n" +
		"size 12345\n")
	oid, ok := ParsePointer(body)
	if !ok {
		t.Fatalf("ParsePointer ok=false, want true")
	}
	const want = "1234567890123456789012345678901234567890123456789012345678901234"
	if oid != want {
		t.Errorf("oid=%q want %q", oid, want)
	}
}

func TestParsePointer_TruncatedAt200Bytes(t *testing.T) {
	// Real LFS pointers are well under 200 bytes; we exercise the cap.
	body := []byte("version https://git-lfs.github.com/spec/v1\n" +
		"oid sha256:1234567890123456789012345678901234567890123456789012345678901234\n" +
		"size 12345\nextra padding line\n")
	if oid, ok := ParsePointer(body); !ok || oid == "" {
		t.Errorf("ParsePointer with trailing junk should still succeed; got ok=%v oid=%q", ok, oid)
	}
}

func TestParsePointer_WrongVersion(t *testing.T) {
	body := []byte("version https://example.com/something/else\noid sha256:abc\n")
	if oid, ok := ParsePointer(body); ok {
		t.Errorf("ParsePointer ok=true oid=%q on wrong version; want ok=false", oid)
	}
}

func TestParsePointer_MissingOID(t *testing.T) {
	body := []byte("version https://git-lfs.github.com/spec/v1\nsize 12345\n")
	if oid, ok := ParsePointer(body); ok {
		t.Errorf("ParsePointer ok=true oid=%q with missing oid line; want ok=false", oid)
	}
}

func TestParsePointer_NonHexOID(t *testing.T) {
	body := []byte("version https://git-lfs.github.com/spec/v1\noid sha256:not-hex!!\nsize 1\n")
	if oid, ok := ParsePointer(body); ok {
		t.Errorf("ParsePointer ok=true oid=%q with non-hex; want ok=false", oid)
	}
}

func TestParsePointer_RandomBinary(t *testing.T) {
	body := []byte{0x00, 0xff, 0x7f, 0x01, 0x02, 0x03}
	if oid, ok := ParsePointer(body); ok {
		t.Errorf("ParsePointer ok=true oid=%q on random binary; want ok=false", oid)
	}
}

func TestParsePointer_OIDTooShort(t *testing.T) {
	body := []byte("version https://git-lfs.github.com/spec/v1\n" +
		"oid sha256:abc123\nsize 1\n")
	if oid, ok := ParsePointer(body); ok {
		t.Errorf("ParsePointer ok=true oid=%q with short hex; want ok=false (must be 64 chars)", oid)
	}
}

func TestParsePointer_OIDUppercase(t *testing.T) {
	// Reject uppercase to keep the live-set key space canonical.
	body := []byte("version https://git-lfs.github.com/spec/v1\n" +
		"oid sha256:1234567890ABCDEF1234567890ABCDEF1234567890ABCDEF1234567890ABCDEF\nsize 1\n")
	if oid, ok := ParsePointer(body); ok {
		t.Errorf("ParsePointer ok=true oid=%q on uppercase hex; want ok=false (must be lowercase)", oid)
	}
}
