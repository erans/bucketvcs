package lfs

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteError_Shape(t *testing.T) {
	w := httptest.NewRecorder()
	WriteError(w, http.StatusUnauthorized, "unauthorized")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("Code=%d", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != ContentType {
		t.Errorf("Content-Type=%q want %q", got, ContentType)
	}
	var body struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Message != "unauthorized" {
		t.Errorf("Message=%q", body.Message)
	}
}

func TestContentType_LFSStandard(t *testing.T) {
	// The LFS spec mandates this exact Content-Type on Batch responses.
	if ContentType != "application/vnd.git-lfs+json" {
		t.Fatalf("ContentType=%q", ContentType)
	}
}
