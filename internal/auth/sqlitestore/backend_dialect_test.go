package sqlitestore

import (
	"errors"
	"testing"
)

func TestSqliteDialect_Identity(t *testing.T) {
	b := sqliteBackend{}
	if got := b.Rebind("SELECT ? WHERE x = ?"); got != "SELECT ? WHERE x = ?" {
		t.Fatalf("sqlite Rebind must be identity, got %q", got)
	}
	if b.NowSeconds() != "strftime('%s','now')" {
		t.Fatalf("sqlite NowSeconds = %q", b.NowSeconds())
	}
	if got := b.Greatest("used_bytes - ?", "0"); got != "MAX(used_bytes - ?, 0)" {
		t.Fatalf("sqlite Greatest = %q", got)
	}
}

func TestSqliteDialect_Classifiers(t *testing.T) {
	b := sqliteBackend{}
	if !b.IsUniqueViolation(errors.New("UNIQUE constraint failed: users.name")) {
		t.Fatal("expected unique violation match")
	}
	if !b.IsCheckViolation(errors.New("CHECK constraint failed: ck")) {
		t.Fatal("expected check violation match")
	}
	if b.IsUniqueViolation(errors.New("syntax error")) {
		t.Fatal("false positive unique")
	}
}

func TestSupportsSkipLocked(t *testing.T) {
	if (sqliteBackend{}).SupportsSkipLocked() {
		t.Fatal("sqlite must not support SKIP LOCKED")
	}
	if (libsqlBackend{}).SupportsSkipLocked() {
		t.Fatal("libsql must not support SKIP LOCKED")
	}
	if !(postgresBackend{}).SupportsSkipLocked() {
		t.Fatal("postgres must support SKIP LOCKED")
	}
}
