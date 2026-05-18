package s3compat

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestSignedGetURLForm(t *testing.T) {
	s, _ := newMockBackend(t)
	got, err := s.SignedGetURL(context.Background(), "k", storage.SignedURLOptions{
		Expires: 5 * time.Minute,
		Method:  "GET",
	})
	if err != nil {
		t.Fatalf("SignedGetURL: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("URL parse: %v", err)
	}
	if !strings.HasSuffix(u.Path, "/k") {
		t.Fatalf("URL path = %q, want suffix /k", u.Path)
	}
	q := u.Query()
	if q.Get("X-Amz-Signature") == "" {
		t.Fatalf("expected X-Amz-Signature in presigned URL")
	}
	if q.Get("X-Amz-Expires") == "" {
		t.Fatalf("expected X-Amz-Expires in presigned URL")
	}
}

func TestSignedGetURLDefaultsTTL(t *testing.T) {
	s, _ := newMockBackend(t)
	got, err := s.SignedGetURL(context.Background(), "k", storage.SignedURLOptions{Method: "GET"})
	if err != nil {
		t.Fatalf("SignedGetURL: %v", err)
	}
	u, _ := url.Parse(got)
	exp := u.Query().Get("X-Amz-Expires")
	if exp == "" || exp == "0" {
		t.Fatalf("default TTL not applied; X-Amz-Expires = %q", exp)
	}
}

func TestSignedGetURLRejectsInvalidKey(t *testing.T) {
	s, _ := newMockBackend(t)
	bad := []string{"", "/foo", "foo/", "foo\x00bar"}
	for _, k := range bad {
		t.Run(k, func(t *testing.T) {
			_, err := s.SignedGetURL(context.Background(), k, storage.SignedURLOptions{Method: "GET"})
			if !errors.Is(err, storage.ErrInvalidArgument) {
				t.Fatalf("SignedGetURL(%q) err = %v, want ErrInvalidArgument", k, err)
			}
		})
	}
}

func TestSignedGetURLAppliesPrefix(t *testing.T) {
	// Verify configured prefix is prepended to the wire key in the URL.
	s, _, srv := newMockBackendWithPrefix(t, "acme/")
	_ = srv
	got, err := s.SignedGetURL(context.Background(), "foo", storage.SignedURLOptions{Method: "GET"})
	if err != nil {
		t.Fatalf("SignedGetURL: %v", err)
	}
	u, _ := url.Parse(got)
	if !strings.HasSuffix(u.Path, "/acme/foo") {
		t.Fatalf("URL path = %q, want suffix /acme/foo", u.Path)
	}
}

func TestSignedGetURL_ExpectedHash_AddsChecksumMode(t *testing.T) {
	// newMockBackendStrictChecksum sets ResponseChecksumValidation=WhenRequired,
	// disabling the AWS SDK default (WhenSupported) that would otherwise inject
	// X-Amz-Checksum-Mode=ENABLED unconditionally. With that default disabled,
	// the adapter's explicit `in.ChecksumMode = types.ChecksumModeEnabled` set
	// in signed.go is the sole source of the header — this test genuinely guards
	// that code path.
	s, _ := newMockBackendStrictChecksum(t)
	raw, err := s.SignedGetURL(context.Background(), "k", storage.SignedURLOptions{
		Expires:      30 * time.Second,
		Method:       "GET",
		ExpectedHash: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatalf("SignedGetURL: %v", err)
	}
	u, perr := url.Parse(raw)
	if perr != nil {
		t.Fatalf("parse url: %v", perr)
	}
	mode := u.Query().Get("X-Amz-Checksum-Mode")
	if !strings.EqualFold(mode, "ENABLED") {
		t.Errorf("expected X-Amz-Checksum-Mode=ENABLED in presigned URL; got %q", mode)
	}
}

func TestSignedGetURL_NoExpectedHash_NoChecksumMode(t *testing.T) {
	// With no ExpectedHash the adapter must not inject ChecksumMode.
	// newMockBackendStrictChecksum sets ResponseChecksumValidation=WhenRequired,
	// disabling the AWS SDK default that would otherwise add the header anyway.
	s, _ := newMockBackendStrictChecksum(t)
	raw, err := s.SignedGetURL(context.Background(), "k", storage.SignedURLOptions{
		Expires: 30 * time.Second,
		Method:  "GET",
	})
	if err != nil {
		t.Fatalf("SignedGetURL: %v", err)
	}
	u, perr := url.Parse(raw)
	if perr != nil {
		t.Fatalf("parse url: %v", perr)
	}
	if mode := u.Query().Get("X-Amz-Checksum-Mode"); mode != "" {
		t.Errorf("did not expect X-Amz-Checksum-Mode without ExpectedHash; got %q", mode)
	}
}

func TestSignedURL_PUT_HasSignature(t *testing.T) {
	s, _ := newMockBackend(t)
	got, err := s.SignedGetURL(context.Background(), "k", storage.SignedURLOptions{
		Method:  "PUT",
		Expires: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("SignedGetURL(PUT): %v", err)
	}
	if !strings.Contains(got, "X-Amz-Signature=") {
		t.Fatalf("expected X-Amz-Signature in presigned URL; got %q", got)
	}
}

func TestSignedURL_PUT_PutObjectRoundTrip(t *testing.T) {
	s, _ := newMockBackend(t)
	url, err := s.SignedGetURL(context.Background(), "k-put", storage.SignedURLOptions{
		Method:  "PUT",
		Expires: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("SignedGetURL(PUT): %v", err)
	}
	req, _ := http.NewRequest(http.MethodPut, url, strings.NewReader("hello"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("PUT status: %d", resp.StatusCode)
	}
	meta, err := s.Head(context.Background(), "k-put")
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if meta.Size != 5 {
		t.Fatalf("Head size = %d, want 5", meta.Size)
	}
}

func TestSignedURL_RejectsUnknownMethod(t *testing.T) {
	s, _ := newMockBackend(t)
	_, err := s.SignedGetURL(context.Background(), "k", storage.SignedURLOptions{
		Method:  "DELETE",
		Expires: 5 * time.Minute,
	})
	if !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("err = %v, want ErrInvalidArgument", err)
	}
}
