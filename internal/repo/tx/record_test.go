package tx_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/repo/tx"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

func newStore(t *testing.T) storage.ObjectStore {
	t.Helper()
	s, err := localfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMarshal_HeaderKeysAtTopLevel(t *testing.T) {
	header := tx.Header{
		SchemaVersion:             1,
		TxID:                      "tx_01HW7",
		RepoID:                    "r_1",
		BaseManifestVersion:       42,
		BaseManifestObjectVersion: "abcd",
		StartedAt:                 time.Date(2026, 5, 3, 20, 0, 0, 0, time.UTC),
	}
	body := tx.Body{
		Type:       "push",
		Actor:      "u_1",
		RefUpdates: json.RawMessage(`[{"ref":"refs/heads/main"}]`),
		NewPacks:   json.RawMessage(`[{"pack_key":"x"}]`),
	}
	out, err := tx.Marshal(header, body)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{
		"schema_version", "tx_id", "repo_id",
		"base_manifest_version", "base_manifest_object_version",
		"started_at", "type", "actor", "ref_updates", "new_packs",
	} {
		if _, ok := raw[k]; !ok {
			t.Errorf("missing top-level key %q in %s", k, out)
		}
	}
}

func TestMarshal_RejectsHeaderKeyInBody(t *testing.T) {
	header := tx.Header{SchemaVersion: 1, TxID: "x", RepoID: "r"}
	body := tx.Body{Type: "push", Actor: "u", Extra: json.RawMessage(`{"tx_id":"hijack"}`)}
	if _, err := tx.Marshal(header, body); err == nil {
		t.Fatal("expected error when body Extra contains reserved header key")
	}
}

func TestMarshal_RejectsExtraOverlapWithKnownBody(t *testing.T) {
	header := tx.Header{SchemaVersion: 1, TxID: "x", RepoID: "r"}
	body := tx.Body{Type: "push", Actor: "u", Extra: json.RawMessage(`{"actor":"hijack"}`)}
	if _, err := tx.Marshal(header, body); err == nil {
		t.Fatal("expected error when Extra contains a known body key")
	}
}

func TestMarshal_RejectsNonObjectExtra(t *testing.T) {
	header := tx.Header{SchemaVersion: 1, TxID: "x", RepoID: "r"}
	body := tx.Body{Type: "push", Actor: "u", Extra: json.RawMessage(`null`)}
	if _, err := tx.Marshal(header, body); err == nil {
		t.Fatal("expected error when Extra is JSON null")
	}
}

func TestMarshal_RejectsExtraKnownBodyKeyEvenWhenBodyFieldOmitted(t *testing.T) {
	// Body.RefUpdates is nil/omitted, but caller smuggles it via Extra.
	// Marshal must reject — Extra cannot supply known body fields.
	header := tx.Header{SchemaVersion: 1, TxID: "x", RepoID: "r"}
	body := tx.Body{
		Type:  "push",
		Actor: "u",
		Extra: json.RawMessage(`{"ref_updates":[{"smuggled":true}]}`),
	}
	if _, err := tx.Marshal(header, body); err == nil {
		t.Fatal("expected error: ref_updates is a known body key, must not be supplied via Extra even when body field is omitted")
	}
}


func TestWrite_PutIfAbsentSemantics(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	key := "tenants/a/repos/b/tx/tx_01HW7.json"

	header := tx.Header{
		SchemaVersion: 1, TxID: "tx_01HW7", RepoID: "b", StartedAt: time.Now().UTC(),
	}
	body := tx.Body{Type: "create", Actor: "u_1"}

	if err := tx.Write(ctx, s, key, header, body); err != nil {
		t.Fatal(err)
	}
	if err := tx.Write(ctx, s, key, header, body); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Errorf("want ErrAlreadyExists on second write, got %v", err)
	}
}
