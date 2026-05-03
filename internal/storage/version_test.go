package storage

import "testing"

func TestVersionKindString(t *testing.T) {
	cases := []struct {
		k    VersionKind
		want string
	}{
		{VersionUnknown, "unknown"},
		{VersionEtag, "etag"},
		{VersionGeneration, "generation"},
		{VersionVersionID, "version_id"},
		{VersionOpaque, "opaque"},
		{VersionKind(99), "unknown"},
	}
	for _, c := range cases {
		if got := c.k.String(); got != c.want {
			t.Errorf("VersionKind(%d).String() = %q, want %q", c.k, got, c.want)
		}
	}
}
