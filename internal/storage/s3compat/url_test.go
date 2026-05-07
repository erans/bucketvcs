package s3compat

import (
	"strings"
	"testing"
)

func TestParseURL(t *testing.T) {
	tests := []struct {
		in            string
		wantScheme    string
		wantBucket    string
		wantPrefix    string
		wantPathStyle bool
		wantErr       string
	}{
		{"s3://my-bucket", "s3", "my-bucket", "", false, ""},
		{"s3://my-bucket/data", "s3", "my-bucket", "data/", false, ""},
		{"s3://my-bucket/data/sub", "s3", "my-bucket", "data/sub/", false, ""},
		{"r2://repo-bucket", "r2", "repo-bucket", "", true, ""},
		{"r2://repo-bucket/p", "r2", "repo-bucket", "p/", true, ""},
		{"s3://", "", "", "", false, "bucket"},
		{"s3:", "", "", "", false, "bucket"},
		{"http://x", "", "", "", false, "scheme"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			c, err := ParseURL(tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got nil", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.scheme != tc.wantScheme {
				t.Fatalf("scheme = %q, want %q", c.scheme, tc.wantScheme)
			}
			if c.Bucket != tc.wantBucket {
				t.Fatalf("Bucket = %q, want %q", c.Bucket, tc.wantBucket)
			}
			if c.Prefix != tc.wantPrefix {
				t.Fatalf("Prefix = %q, want %q", c.Prefix, tc.wantPrefix)
			}
			if c.ForcePathStyle != tc.wantPathStyle {
				t.Fatalf("ForcePathStyle = %v, want %v", c.ForcePathStyle, tc.wantPathStyle)
			}
		})
	}
}

func TestParseURLR2DefaultsRegion(t *testing.T) {
	c, err := ParseURL("r2://b")
	if err != nil {
		t.Fatal(err)
	}
	if c.Region != "auto" {
		t.Fatalf("R2 default region = %q, want \"auto\"", c.Region)
	}
}

func TestParseURL_CredentialErrorDoesNotLeakSecret(t *testing.T) {
	const secret = "supersecret123"
	_, err := ParseURL("s3://access:" + secret + "@bucket/prefix")
	if err == nil {
		t.Fatalf("ParseURL must reject credential-bearing URL")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error %q leaked the secret", err.Error())
	}
	if strings.Contains(err.Error(), "access") {
		t.Fatalf("error %q leaked the access key", err.Error())
	}
}

func TestParseURL_RejectsCredentials(t *testing.T) {
	bad := []string{
		"s3://key:secret@bucket/prefix",
		"r2://key:secret@bucket/prefix",
		"s3://user@bucket",
		"r2://user@bucket",
	}
	for _, u := range bad {
		t.Run(u, func(t *testing.T) {
			_, err := ParseURL(u)
			if err == nil {
				t.Fatalf("ParseURL(%q) must reject userinfo", u)
			}
			if !strings.Contains(err.Error(), "credential") {
				t.Fatalf("error %q does not mention credentials", err.Error())
			}
		})
	}
}
