package s3compat_test

import (
	"encoding/json"
	"github.com/bucketvcs/bucketvcs/internal/storage/s3compat"
	"testing"
)

func TestApplyCredsJSON_S3(t *testing.T) {
	cfg, err := s3compat.ParseURL("s3://mybucket/prefix")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{
		"access_key_id": "AKID", "secret_access_key": "SECRET",
		"session_token": "TOKEN", "region": "eu-west-1", "profile": "myprofile",
	})
	if err := cfg.ApplyCredsJSON(raw); err != nil {
		t.Fatalf("ApplyCredsJSON: %v", err)
	}
	if cfg.AccessKeyID != "AKID" || cfg.SecretAccessKey != "SECRET" ||
		cfg.SessionToken != "TOKEN" || cfg.Region != "eu-west-1" || cfg.Profile != "myprofile" {
		t.Fatalf("field mismatch: %+v", cfg)
	}
	if cfg.Bucket != "mybucket" {
		t.Fatalf("Bucket changed: %s", cfg.Bucket)
	}
	// Unknown keys must not error.
	raw2, _ := json.Marshal(map[string]string{"unknown": "x"})
	if err := cfg.ApplyCredsJSON(raw2); err != nil {
		t.Fatalf("unknown key must not error: %v", err)
	}
}
