package policy

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestEmitRefRejected_Shape(t *testing.T) {
	var buf bytes.Buffer
	perr := &PolicyError{
		Refname:        "refs/heads/main",
		MatchedPattern: "refs/heads/main",
		Reason:         "non-fast-forward push blocked",
		OldOID:         "deadbeef00000000000000000000000000000000",
		NewOID:         "cafebabe00000000000000000000000000000000",
	}
	EmitRefRejected(context.Background(), captureLogger(&buf), "acme", "site", perr, "alice")
	line := buf.String()
	for _, want := range []string{
		`"msg":"policy.ref.rejected"`,
		`"event":"policy.ref.rejected"`,
		`"tenant":"acme"`,
		`"repo":"site"`,
		`"refname":"refs/heads/main"`,
		`"matched_pattern":"refs/heads/main"`,
		`"reason":"non-fast-forward push blocked"`,
		`"actor":"alice"`,
		`"old_oid":"deadbeef00000000000000000000000000000000"`,
		`"new_oid":"cafebabe00000000000000000000000000000000"`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in %s", want, line)
		}
	}
}

func TestEmitRefRejected_NilPolicyErrorIsNoOp(t *testing.T) {
	var buf bytes.Buffer
	EmitRefRejected(context.Background(), captureLogger(&buf), "acme", "site", nil, "alice")
	if buf.Len() != 0 {
		t.Errorf("nil PolicyError emitted: %s", buf.String())
	}
}
