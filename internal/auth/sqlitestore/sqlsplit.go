package sqlitestore

import "strings"

// splitSQLStatements splits a migration file body into individual statements
// for backends (libSQL/HTTP) that execute one statement per Exec.
//
// It is intentionally conservative and handles exactly the SQL our migration
// files use: statement-terminating ';', line comments ('-- …'), and string
// literals ('…') which may themselves contain ';'. Our migrations contain NO
// trigger / BEGIN…END blocks (a compound-statement case this splitter does
// NOT handle); TestSplitSQLStatements_AllMigrationsNonEmpty guards that
// assumption. Trailing/empty/comment-only fragments are dropped.
func splitSQLStatements(body string) []string {
	var stmts []string
	var cur strings.Builder
	inLineComment := false
	inString := false

	runes := []rune(body)
	for i := 0; i < len(runes); i++ {
		c := runes[i]

		if inLineComment {
			cur.WriteRune(c)
			if c == '\n' {
				inLineComment = false
			}
			continue
		}
		if inString {
			cur.WriteRune(c)
			if c == '\'' {
				// '' is an escaped quote inside a string literal.
				if i+1 < len(runes) && runes[i+1] == '\'' {
					cur.WriteRune(runes[i+1])
					i++
					continue
				}
				inString = false
			}
			continue
		}

		switch {
		case c == '-' && i+1 < len(runes) && runes[i+1] == '-':
			inLineComment = true
			cur.WriteRune(c)
		case c == '\'':
			inString = true
			cur.WriteRune(c)
		case c == ';':
			if s := normalizeStmt(cur.String()); s != "" {
				stmts = append(stmts, s)
			}
			cur.Reset()
		default:
			cur.WriteRune(c)
		}
	}
	if s := normalizeStmt(cur.String()); s != "" {
		stmts = append(stmts, s)
	}
	return stmts
}

// normalizeStmt trims whitespace, strips leading comment/blank lines, and
// drops fragments that are empty or consist only of line comments (so a
// comment block between statements is not exec'd). The returned string, if
// non-empty, is guaranteed not to start with a '--' line.
func normalizeStmt(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	// Drop leading blank/comment-only lines.
	for len(lines) > 0 {
		l := strings.TrimSpace(lines[0])
		if l == "" || strings.HasPrefix(l, "--") {
			lines = lines[1:]
		} else {
			break
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
