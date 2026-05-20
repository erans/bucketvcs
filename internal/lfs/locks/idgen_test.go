package locks

import (
	"strings"
	"testing"
)

func TestGenerateLockID_Format(t *testing.T) {
	id, err := generateLockID()
	if err != nil {
		t.Fatalf("generateLockID: %v", err)
	}
	if !strings.HasPrefix(id, "lock_") {
		t.Errorf("id=%q missing 'lock_' prefix", id)
	}
	if got := len(id); got != len("lock_")+26 {
		t.Errorf("len(id)=%d want %d", got, len("lock_")+26)
	}
}

func TestGenerateLockID_Unique(t *testing.T) {
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		id, err := generateLockID()
		if err != nil {
			t.Fatalf("generateLockID: %v", err)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id %q at iteration %d", id, i)
		}
		seen[id] = struct{}{}
	}
}
