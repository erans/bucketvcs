package main

import (
	"strings"
	"testing"
)

func TestParseStoreURL_LocalFS(t *testing.T) {
	cases := []struct {
		url, wantPath string
	}{
		{"localfs:/tmp/x", "/tmp/x"},
		{"localfs:./relative", "./relative"},
		{"localfs:" + strings.Repeat("a", 200), strings.Repeat("a", 200)},
	}
	for _, c := range cases {
		scheme, path, err := parseStoreURL(c.url)
		if err != nil {
			t.Errorf("%q: %v", c.url, err)
			continue
		}
		if scheme != "localfs" || path != c.wantPath {
			t.Errorf("%q: got (%q,%q), want (localfs, %q)", c.url, scheme, path, c.wantPath)
		}
	}
}

func TestParseStoreURL_Errors(t *testing.T) {
	cases := []string{"", "localfs", "http://x", "localfs:"}
	for _, in := range cases {
		if _, _, err := parseStoreURL(in); err == nil {
			t.Errorf("%q: want error", in)
		}
	}
}

func TestParseStoreURL_S3(t *testing.T) {
	scheme, path, err := parseStoreURL("s3://my-bucket/data")
	if err != nil {
		t.Fatalf("parseStoreURL: %v", err)
	}
	if scheme != "s3" {
		t.Fatalf("scheme = %q, want s3", scheme)
	}
	if path != "my-bucket/data" {
		t.Fatalf("path = %q, want my-bucket/data", path)
	}
}

func TestParseStoreURL_R2(t *testing.T) {
	scheme, path, err := parseStoreURL("r2://my-bucket")
	if err != nil {
		t.Fatalf("parseStoreURL: %v", err)
	}
	if scheme != "r2" {
		t.Fatalf("scheme = %q, want r2", scheme)
	}
	if path != "my-bucket" {
		t.Fatalf("path = %q, want my-bucket", path)
	}
}

func TestParseStoreURL_RejectsCloudReservations(t *testing.T) {
	for _, scheme := range []string{"azureblob"} {
		_, _, err := parseStoreURL(scheme + "://x")
		if err == nil {
			t.Fatalf("%s:// must still be reserved", scheme)
		}
		if !strings.Contains(err.Error(), "M7") {
			t.Fatalf("%s:// error %q does not mention M7", scheme, err.Error())
		}
	}
}

func TestParseStoreURL_GCS(t *testing.T) {
	cases := []struct {
		url        string
		wantScheme string
		wantPath   string
	}{
		{"gcs://my-bucket", "gcs", "my-bucket"},
		{"gcs://my-bucket/data", "gcs", "my-bucket/data"},
		{"gcs://my-bucket/data/sub", "gcs", "my-bucket/data/sub"},
		{"gcs://bucket-with-dashes", "gcs", "bucket-with-dashes"},
	}
	for _, c := range cases {
		scheme, path, err := parseStoreURL(c.url)
		if err != nil {
			t.Errorf("%q: unexpected error: %v", c.url, err)
			continue
		}
		if scheme != c.wantScheme {
			t.Errorf("%q: scheme = %q, want %q", c.url, scheme, c.wantScheme)
		}
		if path != c.wantPath {
			t.Errorf("%q: path = %q, want %q", c.url, path, c.wantPath)
		}
	}
}

func TestParseStoreURL_GCS_Errors(t *testing.T) {
	for _, url := range []string{"gcs:///prefix", "gcs://", "gcs://"} {
		_, _, err := parseStoreURL(url)
		if err == nil {
			t.Fatalf("parseStoreURL(%q) must reject empty bucket", url)
		}
	}
}

func TestParseStoreURL_RejectsEmptyBucket(t *testing.T) {
	for _, url := range []string{"s3:///prefix", "r2:///prefix", "s3://", "r2://"} {
		_, _, err := parseStoreURL(url)
		if err == nil {
			t.Fatalf("parseStoreURL(%q) must reject empty bucket", url)
		}
		if !strings.Contains(err.Error(), "bucket") {
			t.Fatalf("parseStoreURL(%q) error %q does not mention 'bucket'", url, err.Error())
		}
	}
}
