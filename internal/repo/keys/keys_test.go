package keys_test

import (
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
)

func TestNewRepo_ValidIDs(t *testing.T) {
	cases := []string{"a", "abc", "acme-prod_1", "A1Z9_-", repeatChar("x", 128)}
	for _, id := range cases {
		if _, err := keys.NewRepo(id, id); err != nil {
			t.Errorf("expected NewRepo(%q,%q) ok, got %v", id, id, err)
		}
	}
}

func TestNewRepo_InvalidIDs(t *testing.T) {
	cases := []struct {
		tenant, repo string
		wantErr      error
	}{
		{"", "ok", repo.ErrInvalidTenantID},
		{"ok", "", repo.ErrInvalidRepoID},
		{"a/b", "ok", repo.ErrInvalidTenantID},
		{"ok", "a..b", repo.ErrInvalidRepoID},
		{"ok", "a b", repo.ErrInvalidRepoID},
		{repeatChar("x", 129), "ok", repo.ErrInvalidTenantID},
		{"ok", repeatChar("x", 129), repo.ErrInvalidRepoID},
		{"ok", ".", repo.ErrInvalidRepoID},
		{"ok", "..", repo.ErrInvalidRepoID},
	}
	for _, c := range cases {
		_, err := keys.NewRepo(c.tenant, c.repo)
		if !errors.Is(err, c.wantErr) {
			t.Errorf("NewRepo(%q,%q): want %v, got %v", c.tenant, c.repo, c.wantErr, err)
		}
	}
}

func TestRepoPrefix(t *testing.T) {
	r, err := keys.NewRepo("acme", "my-repo")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := r.Prefix(), "tenants/acme/repos/my-repo/"; got != want {
		t.Errorf("Prefix: want %q, got %q", want, got)
	}
}

func repeatChar(c string, n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = c[0]
	}
	return string(out)
}
