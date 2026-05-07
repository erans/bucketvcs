package s3compat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// httptestServer returns a server that responds 404 to any request.
// It is the simplest mock substrate for asserting that Open() wires
// the SDK client to a configurable endpoint (no AWS reachability
// required). Per-method tests in T7+ use newMockBackend instead.
func httptestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOpenAppliesPathStyle(t *testing.T) {
	srv := httptestServer(t)
	cfg := Config{
		Bucket:          "test-bucket",
		Region:          "us-east-1",
		Endpoint:        srv.URL,
		ForcePathStyle:  true,
		AccessKeyID:     "AKID",
		SecretAccessKey: "SECRET",
	}
	s, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if s.client == nil {
		t.Fatalf("client not initialized")
	}
	if s.presign == nil {
		t.Fatalf("presign client not initialized")
	}
	// Ensure a real request goes to the test server (not AWS).
	_, _ = s.Head(context.Background(), "anything")
}

func TestOpenRejectsBadConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"missing bucket", Config{Region: "us-east-1"}, "bucket"},
		{"missing region", Config{Bucket: "b"}, "region"},
		{"r2 without endpoint", Config{Bucket: "b", Region: "auto", scheme: "r2"}, "Endpoint"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Open(context.Background(), tc.cfg)
			if err == nil {
				t.Fatalf("want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %q does not mention %q", err.Error(), tc.want)
			}
		})
	}
}
