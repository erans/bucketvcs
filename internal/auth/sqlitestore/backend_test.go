package sqlitestore

import "testing"

func TestResolveBackend_Selection(t *testing.T) {
	cases := []struct {
		value    string
		wantName string
	}{
		{"/var/lib/bucketvcs/auth.db", "sqlite"},
		{"auth.db", "sqlite"},
		{"sqlite:/tmp/a.db", "sqlite"},
		{"file:/tmp/a.db", "sqlite"},
		{`C:\data\auth.db`, "sqlite"}, // Windows drive path, not a URL scheme
		{"libsql://db.turso.io", "libsql"},
		{"https://db.turso.io", "libsql"},
	}
	t.Setenv(dbAuthTokenEnv, "") // token is optional; selection must not depend on it
	for _, c := range cases {
		b, err := resolveBackend(c.value)
		if err != nil {
			t.Fatalf("%s: %v", c.value, err)
		}
		if b.Name() != c.wantName {
			t.Fatalf("%s: backend=%s want %s", c.value, b.Name(), c.wantName)
		}
	}
}

func TestResolveBackend_LibsqlNoTokenOK(t *testing.T) {
	// Token is optional (self-hosted sqld may run without auth); a libsql URL
	// with no token must still resolve, not error.
	t.Setenv(dbAuthTokenEnv, "")
	b, err := resolveBackend("http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("no-token libsql URL should resolve: %v", err)
	}
	if b.Name() != "libsql" {
		t.Fatalf("backend=%s want libsql", b.Name())
	}
}

func TestResolveBackend_TokenFromURLAllowedWithoutEnv(t *testing.T) {
	t.Setenv(dbAuthTokenEnv, "")
	b, err := resolveBackend("libsql://db.turso.io?authToken=abc")
	if err != nil {
		t.Fatalf("token in URL should be accepted: %v", err)
	}
	if b.Name() != "libsql" {
		t.Fatalf("backend=%s", b.Name())
	}
}

func TestSqlitePath_StripsScheme(t *testing.T) {
	if got := sqlitePath("sqlite:/tmp/a.db"); got != "/tmp/a.db" {
		t.Fatalf("sqlitePath sqlite: = %q", got)
	}
	if got := sqlitePath("/tmp/a.db"); got != "/tmp/a.db" {
		t.Fatalf("sqlitePath bare = %q", got)
	}
}
