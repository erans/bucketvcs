package refstore_test

import (
	"context"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/repo/refstore"
)

func TestInline_Lookup_Hit(t *testing.T) {
	body := &manifest.Body{Refs: map[string]string{"refs/heads/main": "aa"}}
	rs, err := refstore.New(context.Background(), nil, nil, body)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if rs.Mode() != refstore.ModeInline {
		t.Errorf("Mode=%v want inline", rs.Mode())
	}
	oid, ok, err := rs.Lookup(context.Background(), "refs/heads/main")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !ok || oid != "aa" {
		t.Errorf("Lookup=(%q, %v)", oid, ok)
	}
}

func TestInline_Lookup_Miss(t *testing.T) {
	body := &manifest.Body{Refs: map[string]string{"refs/heads/main": "aa"}}
	rs, _ := refstore.New(context.Background(), nil, nil, body)
	oid, ok, err := rs.Lookup(context.Background(), "refs/heads/absent")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if ok || oid != "" {
		t.Errorf("Lookup=(%q, %v)", oid, ok)
	}
}

func TestInline_List(t *testing.T) {
	in := map[string]string{
		"refs/heads/main": "aa",
		"refs/heads/dev":  "bb",
		"refs/tags/v1":    "cc",
	}
	rs, _ := refstore.New(context.Background(), nil, nil, &manifest.Body{Refs: in})
	out, err := rs.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("List len=%d want 3", len(out))
	}
	for k, v := range in {
		if got := out[k]; got != v {
			t.Errorf("List[%q]=%q want %q", k, got, v)
		}
	}
}

func TestInline_Stage_AddDelete(t *testing.T) {
	body := &manifest.Body{Refs: map[string]string{
		"refs/heads/main": "aa",
		"refs/heads/del":  "bb",
	}}
	rs, _ := refstore.New(context.Background(), nil, nil, body)
	updates := map[string]string{
		"refs/heads/dev": "cc", // create
		"refs/heads/del": "",   // delete (empty oid)
	}
	stage, err := rs.Stage(context.Background(), updates)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if stage.Mode != refstore.ModeInline {
		t.Errorf("Mode=%v want inline", stage.Mode)
	}
	if len(stage.NewShardObjects) != 0 {
		t.Errorf("NewShardObjects=%v want empty", stage.NewShardObjects)
	}
	if len(stage.NewRefShards) != 0 {
		t.Errorf("NewRefShards=%v want empty", stage.NewRefShards)
	}
	want := map[string]string{
		"refs/heads/main": "aa",
		"refs/heads/dev":  "cc",
	}
	if len(stage.NewInlineRefs) != len(want) {
		t.Fatalf("NewInlineRefs len=%d want %d", len(stage.NewInlineRefs), len(want))
	}
	for k, v := range want {
		if got := stage.NewInlineRefs[k]; got != v {
			t.Errorf("NewInlineRefs[%q]=%q want %q", k, got, v)
		}
	}
}

func TestInline_Stage_NullOIDIsDelete(t *testing.T) {
	const nullOID = "0000000000000000000000000000000000000000"
	body := &manifest.Body{Refs: map[string]string{"refs/heads/del": "aa"}}
	rs, _ := refstore.New(context.Background(), nil, nil, body)
	stage, err := rs.Stage(context.Background(), map[string]string{"refs/heads/del": nullOID})
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if _, present := stage.NewInlineRefs["refs/heads/del"]; present {
		t.Errorf("nullOID should delete; got %+v", stage.NewInlineRefs)
	}
}

func TestInline_Stage_DoesNotMutateInputBody(t *testing.T) {
	orig := map[string]string{"refs/heads/main": "aa"}
	body := &manifest.Body{Refs: orig}
	rs, _ := refstore.New(context.Background(), nil, nil, body)
	if _, err := rs.Stage(context.Background(), map[string]string{"refs/heads/main": "bb"}); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if orig["refs/heads/main"] != "aa" {
		t.Errorf("Stage mutated input map: %+v", orig)
	}
}
