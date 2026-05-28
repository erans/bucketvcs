package sqlitestore

import (
	"testing"
)

func TestPostgresMigrationsSplit(t *testing.T) {
	entries, err := postgresMigrations.ReadDir("migrations_postgres")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 10 {
		t.Fatalf("expected 10 postgres migrations, got %d", len(entries))
	}
	for _, e := range entries {
		body, err := postgresMigrations.ReadFile("migrations_postgres/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		stmts := splitSQLStatements(string(body))
		if len(stmts) == 0 {
			t.Fatalf("%s: no statements", e.Name())
		}
		for _, s := range stmts {
			if s == "" {
				t.Fatalf("%s: empty statement", e.Name())
			}
		}
	}
}
