package web

import (
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

func TestRelTimeAt(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	cases := map[int64]string{
		0:                                     "—",
		now.Unix():                            "now",
		now.Add(-30 * time.Second).Unix():     "now",
		now.Add(-5 * time.Minute).Unix():      "5m ago",
		now.Add(-2 * time.Hour).Unix():        "2h ago",
		now.Add(-3 * 24 * time.Hour).Unix():   "3d ago",
		now.Add(-70 * 24 * time.Hour).Unix():  "2mo ago",
		now.Add(-800 * 24 * time.Hour).Unix(): "2y ago",
		now.Add(5 * time.Minute).Unix():       "now", // future clock skew clamps to now
	}
	for in, want := range cases {
		if got := relTimeAt(now, in); got != want {
			t.Errorf("relTimeAt(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestAbsTime(t *testing.T) {
	ts := time.Date(2026, 6, 3, 18, 44, 0, 0, time.UTC).Unix()
	if got := absTime(ts); got != "2026-06-03 18:44 UTC" {
		t.Fatalf("absTime = %q", got)
	}
	if got := absTime(0); got != "" {
		t.Fatalf("absTime(0) = %q, want empty", got)
	}
}

func TestHumanSize(t *testing.T) {
	cases := map[int64]string{
		0:                                   "0 B",
		312:                                 "312 B",
		1024:                                "1.0 KiB",
		1228:                                "1.2 KiB",
		10 * 1024:                           "10 KiB",
		4 << 20:                             "4.0 MiB",
		1181116006:                          "1.1 GiB",
		int64(1) << 50:                      "1.0 PiB",
		(int64(1) << 62) + (int64(1) << 61): "6.0 EiB",
	}
	for in, want := range cases {
		if got := humanSize(in); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestDiffClass(t *testing.T) {
	cases := map[byte]string{'+': "add", '-': "del", ' ': "ctx", 'x': "ctx"}
	for in, want := range cases {
		if got := diffClass(in); got != want {
			t.Errorf("diffClass(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestScopeStr(t *testing.T) {
	fns := templateFuncs()
	fn := fns["scopestr"].(func(auth.TokenScope) string)

	cases := []struct {
		scope auth.TokenScope
		want  string
	}{
		{auth.ScopeLegacy, "legacy (full access)"},
		{auth.ScopeRepoRead | auth.ScopeLFSRead, "repo:read,lfs:read"},
		{auth.ScopeRepoAdmin | auth.ScopeRepoWrite | auth.ScopeRepoRead, "repo:admin,repo:write,repo:read"},
		{auth.ScopeWebhookAdmin, "webhook:admin"},
		{auth.ScopeStorageAdmin, "storage:admin"},
	}
	for _, tc := range cases {
		if got := fn(tc.scope); got != tc.want {
			t.Errorf("scopestr(%v) = %q, want %q", tc.scope, got, tc.want)
		}
	}
}
