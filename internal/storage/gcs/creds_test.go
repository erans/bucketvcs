package gcs_test

import (
	"encoding/json"
	"github.com/bucketvcs/bucketvcs/internal/storage/gcs"
	"testing"
)

func TestApplyCredsJSON_GCS(t *testing.T) {
	cfg, err := gcs.ParseURL("gcs://mybucket")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{"service_account_json": `{"type":"service_account"}`})
	if err := cfg.ApplyCredsJSON(raw); err != nil {
		t.Fatalf("ApplyCredsJSON: %v", err)
	}
	if string(cfg.CredentialsJSON) != `{"type":"service_account"}` {
		t.Fatalf("CredentialsJSON mismatch: %s", cfg.CredentialsJSON)
	}
	if cfg.Bucket != "mybucket" {
		t.Fatalf("Bucket changed")
	}
}
