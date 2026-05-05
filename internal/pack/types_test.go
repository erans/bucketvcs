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
		TypeCommit:    "commit",
		TypeTree:      "tree",
		TypeBlob:      "blob",
		TypeTag:       "tag",
		typeOFSDelta:  "ofs_delta",
		typeREFDelta:  "ref_delta",
		ObjectType(0): "invalid(0)",
		ObjectType(5): "invalid(5)",
	}
	for typ, want := range cases {
		if got := typ.String(); got != want {
			t.Fatalf("ObjectType(%d).String: got %q, want %q", typ, got, want)
		}
	}
}

func TestParseOID_AcceptsMixedCase(t *testing.T) {
	want := "0123456789abcdef0123456789abcdef01234567"
	upper := "0123456789ABCDEF0123456789ABCDEF01234567"
	lower, err := ParseOID(want)
	if err != nil {
		t.Fatalf("ParseOID lower: %v", err)
	}
	mixed, err := ParseOID(upper)
	if err != nil {
		t.Fatalf("ParseOID upper: %v", err)
	}
	if lower != mixed {
		t.Fatalf("upper and lower hex should parse equal: %v vs %v", lower, mixed)
	}
	// String always lowercase.
	if mixed.String() != want {
		t.Fatalf("String: got %q, want %q", mixed.String(), want)
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
