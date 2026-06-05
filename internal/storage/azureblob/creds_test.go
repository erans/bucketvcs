package azureblob_test

import (
	"encoding/json"
	"github.com/bucketvcs/bucketvcs/internal/storage/azureblob"
	"testing"
)

func TestApplyCredsJSON_Azure(t *testing.T) {
	cfg, err := azureblob.ParseURL("azureblob://mycontainer")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{"account_key": "KEY", "connection_string": "CS"})
	if err := cfg.ApplyCredsJSON(raw); err != nil {
		t.Fatalf("ApplyCredsJSON: %v", err)
	}
	if cfg.AccountKey != "KEY" || cfg.ConnectionString != "CS" {
		t.Fatalf("field mismatch: %+v", cfg)
	}
}
