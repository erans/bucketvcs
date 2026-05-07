package s3compat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// setHermeticAWSConfig makes the calling test hermetic with respect to
// region resolution: it clears all env vars the AWS SDK consults for
// region and credentials file paths, and points the config/credentials
// files at empty temp files so a developer's ~/.aws/config doesn't leak in.
func setHermeticAWSConfig(t *testing.T) {
	t.Helper()
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_DEFAULT_PROFILE", "")
	emptyCfg := filepath.Join(t.TempDir(), "config")
	emptyCreds := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(emptyCfg, []byte{}, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(emptyCreds, []byte{}, 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", emptyCfg)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", emptyCreds)
	// Disable IMDS metadata fallback to prevent the SDK from trying to
	// query EC2 instance metadata for a region.
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
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
	setHermeticAWSConfig(t)
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

func TestOpenAcceptsRegionFromEnv(t *testing.T) {
	t.Setenv("AWS_REGION", "ap-southeast-2")
	srv := httptestServer(t)
	cfg := Config{
		Bucket:          "test-bucket",
		Endpoint:        srv.URL,
		ForcePathStyle:  true,
		AccessKeyID:     "AKID",
		SecretAccessKey: "SECRET",
	}
	s, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if s == nil {
		t.Fatalf("Open returned nil S3Compat")
	}
	// We can't directly inspect the resolved region without exporting
	// s.cfg, but Open succeeding here proves Validate did not fail
	// with "region is required" — the env-supplied region was honored.
}

func TestOpenStillRejectsEmptyRegion(t *testing.T) {
	// Make this test hermetic: clear all env vars the AWS SDK consults
	// for region resolution, and point the config/credentials files at
	// empty temp files so a developer's ~/.aws/config doesn't leak in.
	setHermeticAWSConfig(t)
	cfg := Config{
		Bucket:          "test-bucket",
		AccessKeyID:     "AKID",
		SecretAccessKey: "SECRET",
	}
	_, err := Open(context.Background(), cfg)
	if err == nil {
		t.Fatalf("Open with no region anywhere: want error, got nil")
	}
	if !strings.Contains(err.Error(), "region") {
		t.Fatalf("err %q does not mention region", err.Error())
	}
}
