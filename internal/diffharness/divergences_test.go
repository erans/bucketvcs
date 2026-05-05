package diffharness

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestKnownDivergences_FormatGate parses known-divergences.md and fails
// CI if any entry is missing classification, date, or issue link. Empty
// file is fine (M2 ship state).
func TestKnownDivergences_FormatGate(t *testing.T) {
	path := repoRoot(t) + "/docs/superpowers/diffharness/known-divergences.md"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(string(data), "\n")
	const (
		classKey = "Classification:"
		dateKey  = "Date:"
		issueKey = "Issue:"
	)
	allowedClasses := map[string]bool{
		"bucketvcs bug":                     true,
		"git quirk to emulate":              true,
		"intentional documented difference": true,
		"unsupported optional capability":   true,
		"invalid test case":                 true,
	}
	var inEntry bool
	var sawClass, sawDate, sawIssue bool
	finishEntry := func(title string) {
		if !sawClass {
			t.Fatalf("entry %q missing %s", title, classKey)
		}
		if !sawDate {
			t.Fatalf("entry %q missing %s", title, dateKey)
		}
		if !sawIssue {
			t.Fatalf("entry %q missing %s", title, issueKey)
		}
	}
	var currentTitle string
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "## "):
			if inEntry {
				finishEntry(currentTitle)
			}
			currentTitle = strings.TrimPrefix(line, "## ")
			inEntry = true
			sawClass, sawDate, sawIssue = false, false, false
		case inEntry && strings.HasPrefix(line, classKey):
			val := strings.TrimSpace(strings.TrimPrefix(line, classKey))
			if !allowedClasses[val] {
				t.Fatalf("entry %q: unknown classification %q", currentTitle, val)
			}
			sawClass = true
		case inEntry && strings.HasPrefix(line, dateKey):
			val := strings.TrimSpace(strings.TrimPrefix(line, dateKey))
			if len(val) != 10 || val[4] != '-' || val[7] != '-' {
				t.Fatalf("entry %q: bad %s %q (want YYYY-MM-DD)", currentTitle, dateKey, val)
			}
			sawDate = true
		case inEntry && strings.HasPrefix(line, issueKey):
			val := strings.TrimSpace(strings.TrimPrefix(line, issueKey))
			if !strings.HasPrefix(val, "https://") {
				t.Fatalf("entry %q: %s must start with https://, got %q", currentTitle, issueKey, val)
			}
			sawIssue = true
		}
	}
	if inEntry {
		finishEntry(currentTitle)
	}
}

// repoRoot walks up from this file's location to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed")
	}
	// .../internal/diffharness/divergences_test.go -> repo root is two levels up
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}
