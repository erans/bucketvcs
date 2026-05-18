package lfs

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestBatchRequest_UnmarshalSpecFixture(t *testing.T) {
	body := []byte(`{
        "operation": "upload",
        "transfers": ["basic"],
        "objects": [
            {"oid":"1111111111111111111111111111111111111111111111111111111111111111","size":123},
            {"oid":"2222222222222222222222222222222222222222222222222222222222222222","size":456}
        ]
    }`)
	var req BatchRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if req.Operation != "upload" {
		t.Errorf("Operation=%q", req.Operation)
	}
	if len(req.Transfers) != 1 || req.Transfers[0] != "basic" {
		t.Errorf("Transfers=%v", req.Transfers)
	}
	if len(req.Objects) != 2 {
		t.Fatalf("len(Objects)=%d", len(req.Objects))
	}
	if req.Objects[0].OID != "1111111111111111111111111111111111111111111111111111111111111111" || req.Objects[0].Size != 123 {
		t.Errorf("Objects[0]=%+v", req.Objects[0])
	}
}

func TestBatchResponse_MarshalUploadAction(t *testing.T) {
	expiresAt := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	resp := BatchResponse{
		Transfer: "basic",
		Objects: []ObjectAction{
			{
				OID:  "abcd",
				Size: 100,
				Actions: map[string]Action{
					"upload": {
						Href:      "https://example/put",
						Header:    map[string]string{"Content-Type": "application/octet-stream"},
						ExpiresAt: &expiresAt,
					},
					"verify": {
						Href:   "https://example/verify",
						Header: map[string]string{"Authorization": "Bearer x"},
						// ExpiresAt omitted — Action with nil ExpiresAt
					},
				},
			},
		},
	}
	got, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(got)
	for _, want := range []string{
		`"transfer":"basic"`,
		`"oid":"abcd"`,
		`"size":100`,
		`"upload":{`,
		`"href":"https://example/put"`,
		`"expires_at":"2026-05-18T12:00:00Z"`,
		`"verify":{`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in %s", want, s)
		}
	}
}

func TestAction_ExpiresAt_OmittedWhenNil(t *testing.T) {
	// Verify-action shape: no expiry. Pointer + omitempty should drop
	// the field entirely from the JSON output.
	resp := BatchResponse{
		Transfer: "basic",
		Objects: []ObjectAction{
			{
				OID: "abcd", Size: 1,
				Actions: map[string]Action{
					"verify": {Href: "https://x/verify"},
				},
			},
		},
	}
	got, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(got), "expires_at") {
		t.Fatalf("expires_at should be omitted when nil; got %s", got)
	}
	if strings.Contains(string(got), "0001-01-01") {
		t.Fatalf("zero-time leaked into JSON: %s", got)
	}
}

func TestBatchResponse_ObjectError(t *testing.T) {
	resp := BatchResponse{
		Transfer: "basic",
		Objects: []ObjectAction{
			{OID: "missing-oid", Size: 0, Error: &ObjectError{Code: 404, Message: "not found"}},
		},
	}
	got, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(got)
	for _, want := range []string{`"error":{`, `"code":404`, `"message":"not found"`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in %s", want, s)
		}
	}
	if strings.Contains(s, `"actions":`) {
		t.Errorf("Actions should be omitted when Error is set; got %s", s)
	}
}

func TestVerifyRequest_Unmarshal(t *testing.T) {
	var req VerifyRequest
	if err := json.Unmarshal([]byte(`{"oid":"abc","size":42}`), &req); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if req.OID != "abc" || req.Size != 42 {
		t.Errorf("req=%+v", req)
	}
}
