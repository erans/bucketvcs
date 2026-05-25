package hooks

import (
	"strings"
	"testing"
)

func TestPreReceiveStdin_NativeFormat(t *testing.T) {
	got := string(PreReceiveStdin(PreReceivePayload{
		Updates: []RefUpdate{
			{OldOID: "aaa", NewOID: "bbb", Refname: "refs/heads/main"},
			{OldOID: "0000000000000000000000000000000000000000", NewOID: "ccc", Refname: "refs/heads/feature"},
		},
	}))
	want := "aaa bbb refs/heads/main\n" +
		"0000000000000000000000000000000000000000 ccc refs/heads/feature\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestPostReceiveStdin_LinesThenBlankThenJSON(t *testing.T) {
	got := string(PostReceiveStdin(PostReceivePayload{
		PreReceivePayload: PreReceivePayload{
			Tenant: "acme", Repo: "site", PushID: "uuid-123",
			Updates: []RefUpdate{{OldOID: "a", NewOID: "b", Refname: "refs/heads/main"}},
		},
		TxID:            "tx-7",
		ManifestVersion: 42,
		StorageBackend:  "localfs",
	}))
	parts := strings.SplitN(got, "\n\n", 2)
	if len(parts) != 2 {
		t.Fatalf("expected lines + blank + json, got %q", got)
	}
	if parts[0] != "a b refs/heads/main" {
		t.Errorf("native lines = %q", parts[0])
	}
	if !strings.Contains(parts[1], `"tx_id":"tx-7"`) {
		t.Errorf("JSON missing tx_id: %s", parts[1])
	}
	if !strings.Contains(parts[1], `"manifest_version":42`) {
		t.Errorf("JSON missing manifest_version: %s", parts[1])
	}
}
