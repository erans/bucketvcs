package browsemodel

import (
	"errors"
	"testing"
)

func TestResolveRest_SlashRefVsPath(t *testing.T) {
	const (
		hexA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		hexB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		hexC = "cccccccccccccccccccccccccccccccccccccccc"
		hexD = "dddddddddddddddddddddddddddddddddddddddd"
	)
	refs := Refs{
		Default:  "main",
		Branches: []RefInfo{{Name: "main", OID: hexA}, {Name: "feature/foo", OID: hexB}},
		Tags:     []RefInfo{{Name: "v1.0", OID: hexC}},
	}

	// "feature/foo/c.txt" must split ref="feature/foo", path="c.txt".
	r, err := ResolveRest(refs, "feature/foo/c.txt")
	if err != nil {
		t.Fatalf("ResolveRest slash ref: %v", err)
	}
	if r.Ref != "feature/foo" || r.Path != "c.txt" || r.OID != hexB {
		t.Fatalf("got %+v, want Ref=feature/foo Path=c.txt OID=%s", r, hexB)
	}

	// "main" alone resolves to a ref with empty path.
	r, err = ResolveRest(refs, "main")
	if err != nil || r.Ref != "main" || r.Path != "" || r.OID != hexA {
		t.Fatalf("ResolveRest main: %v %+v", err, r)
	}

	// A raw 40-hex OID resolves with empty ref.
	r, err = ResolveRest(refs, hexD+"/a.txt")
	if err != nil || r.Ref != "" || r.OID != hexD || r.Path != "a.txt" {
		t.Fatalf("ResolveRest oid: %v %+v", err, r)
	}

	// Unknown ref → ErrNotFound.
	if _, err := ResolveRest(refs, "nope/x.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound for unknown ref, got %v", err)
	}
}
