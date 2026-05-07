package s3compat

import (
	"context"
	"errors"
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
