package pack

import (
	"encoding/hex"
	"testing"
)

func TestOID_String_RoundTrip(t *testing.T) {
	want := "0123456789abcdef0123456789abcdef01234567"
	b, err := hex.DecodeString(want)
	if err != nil {
		t.Fatalf("hex: %v", err)
	}
	var oid OID
	copy(oid[:], b)
	got := oid.String()
	if got != want {
		t.Fatalf("OID.String: got %q, want %q", got, want)
	}
	parsed, err := ParseOID(want)
	if err != nil {
		t.Fatalf("ParseOID: %v", err)
	}
	if parsed != oid {
		t.Fatalf("ParseOID round-trip mismatch")
	}
}

func TestParseOID_RejectsBadLengths(t *testing.T) {
	for _, in := range []string{"", "abc", repeat("a", 39), repeat("a", 41)} {
		if _, err := ParseOID(in); err == nil {
			t.Fatalf("ParseOID(%q) should fail", in)
		}
	}
}

func TestParseOID_RejectsNonHex(t *testing.T) {
	in := "0123456789abcdef0123456789abcdef0123456g"
	if _, err := ParseOID(in); err == nil {
		t.Fatalf("ParseOID with non-hex should fail")
	}
}

func TestObjectType_String(t *testing.T) {
	cases := map[ObjectType]string{
		TypeCommit: "commit",
		TypeTree:   "tree",
		TypeBlob:   "blob",
		TypeTag:    "tag",
	}
	for typ, want := range cases {
		if got := typ.String(); got != want {
			t.Fatalf("ObjectType(%d).String: got %q, want %q", typ, got, want)
		}
	}
}

// repeat returns s repeated n times. Local helper.
func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
