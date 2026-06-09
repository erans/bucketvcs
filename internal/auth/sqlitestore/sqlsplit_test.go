package sqlitestore

import (
	"embed"
	"strings"
	"testing"
)

func TestSplitSQLStatements_Basic(t *testing.T) {
	in := `
-- a comment
CREATE TABLE a (x TEXT);

CREATE INDEX a_idx ON a(x);
INSERT INTO a (x) VALUES ('hi');
`
	got := splitSQLStatements(in)
	if len(got) != 3 {
		t.Fatalf("want 3 statements, got %d: %#v", len(got), got)
	}
	for _, s := range got {
		if strings.TrimSpace(s) == "" {
			t.Fatalf("empty statement in %#v", got)
		}
		if strings.HasPrefix(strings.TrimSpace(s), "--") {
			t.Fatalf("comment leaked as statement: %q", s)
		}
	}
}

func TestSplitSQLStatements_AllMigrationsNonEmpty(t *testing.T) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		count++
		body, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		stmts := splitSQLStatements(string(body))
		if len(stmts) == 0 {
			t.Fatalf("%s: split to zero statements", e.Name())
		}
		for _, s := range stmts {
			ts := strings.TrimSpace(s)
			if ts == "" || strings.HasPrefix(ts, "--") {
				t.Fatalf("%s: bad statement %q", e.Name(), s)
			}
		}
	}
	if count != 18 {
		t.Fatalf("expected 18 migration files, saw %d", count)
	}
}

var _ embed.FS // migrationsFS is declared in schema.go
