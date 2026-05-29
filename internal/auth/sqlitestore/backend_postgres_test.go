package sqlitestore

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestPostgresRebind(t *testing.T) {
	b := postgresBackend{}
	cases := map[string]string{
		"SELECT 1":                            "SELECT 1",
		"WHERE a = ?":                         "WHERE a = $1",
		"VALUES (?, ?, ?)":                    "VALUES ($1, $2, $3)",
		"WHERE a = ? AND b = '?lit' OR c = ?": "WHERE a = $1 AND b = '?lit' OR c = $2", // ? in literal untouched
	}
	for in, want := range cases {
		if got := b.Rebind(in); got != want {
			t.Fatalf("Rebind(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPostgresClassifiers(t *testing.T) {
	b := postgresBackend{}
	if !b.IsUniqueViolation(&pgconn.PgError{Code: "23505"}) {
		t.Fatal("23505 should be unique violation")
	}
	if !b.IsCheckViolation(&pgconn.PgError{Code: "23514"}) {
		t.Fatal("23514 should be check violation")
	}
	if b.IsUniqueViolation(errors.New("plain")) {
		t.Fatal("plain error must not classify")
	}
	if !b.IsFingerprintUniqueViolation(&pgconn.PgError{Code: "23505", ConstraintName: "ssh_keys_fingerprint_key"}) {
		t.Fatal("fingerprint constraint should match")
	}
	if b.IsFingerprintUniqueViolation(&pgconn.PgError{Code: "23505", ConstraintName: "users_name_key"}) {
		t.Fatal("non-fingerprint constraint must not match")
	}
}

func TestPostgresDialectForms(t *testing.T) {
	b := postgresBackend{}
	if b.NowSeconds() != "EXTRACT(EPOCH FROM now())::bigint" {
		t.Fatalf("NowSeconds = %q", b.NowSeconds())
	}
	if got := b.Greatest("used_bytes - ?", "0"); got != "GREATEST(used_bytes - ?, 0)" {
		t.Fatalf("Greatest = %q", got)
	}
}

func TestResolveBackend_Postgres(t *testing.T) {
	for _, v := range []string{"postgres://u@h/db", "postgresql://u@h/db"} {
		b, err := resolveBackend(v)
		if err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		if b.Name() != "postgres" {
			t.Fatalf("%s: backend=%s want postgres", v, b.Name())
		}
	}
}

func TestWithMaxConns(t *testing.T) {
	// Default (no option) → postgres uses 10.
	b, err := resolveBackend("postgres://u@h/db")
	if err != nil {
		t.Fatal(err)
	}
	if got := b.(postgresBackend).maxConns; got != 10 {
		t.Fatalf("default maxConns = %d, want 10", got)
	}
	// Explicit option overrides.
	b2, err := resolveBackend("postgres://u@h/db", WithMaxConns(25))
	if err != nil {
		t.Fatal(err)
	}
	if got := b2.(postgresBackend).maxConns; got != 25 {
		t.Fatalf("maxConns = %d, want 25", got)
	}
}
