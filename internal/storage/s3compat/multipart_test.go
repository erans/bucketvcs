package s3compat

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

func TestMultipartRoundtrip(t *testing.T) {
	s, mb := newMockBackend(t)

	up, err := s.CreateMultipart(context.Background(), "big.bin", nil)
	if err != nil {
		t.Fatalf("CreateMultipart: %v", err)
	}
	if up.UploadID() == "" {
		t.Fatalf("UploadID empty")
	}
	if up.Key() != "big.bin" {
		t.Fatalf("Key = %q, want big.bin", up.Key())
	}

	p1, err := up.UploadPart(context.Background(), 1, strings.NewReader("hello "))
	if err != nil {
		t.Fatalf("UploadPart 1: %v", err)
	}
	p2, err := up.UploadPart(context.Background(), 2, strings.NewReader("world"))
	if err != nil {
		t.Fatalf("UploadPart 2: %v", err)
	}

	v, err := s.CompleteMultipartIfAbsent(context.Background(), up, []storage.MultipartPart{p1, p2})
	if err != nil {
		t.Fatalf("CompleteMultipartIfAbsent: %v", err)
	}
	if v.Token == "" {
		t.Fatalf("Token empty")
	}
	if !bytes.Equal(mb.objects["big.bin"].body, []byte("hello world")) {
		t.Fatalf("assembled body = %q, want \"hello world\"", mb.objects["big.bin"].body)
	}
}

func TestCompleteMultipartIfAbsentConflict(t *testing.T) {
	s, mb := newMockBackend(t)
	mb.put("k", []byte("existing"), `"e0"`)

	up, err := s.CreateMultipart(context.Background(), "k", nil)
	if err != nil {
		t.Fatal(err)
	}
	p1, err := up.UploadPart(context.Background(), 1, strings.NewReader("new"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CompleteMultipartIfAbsent(context.Background(), up, []storage.MultipartPart{p1})
	if !errors.Is(err, storage.ErrAlreadyExists) {
		t.Fatalf("err = %v, want ErrAlreadyExists", err)
	}
}

func TestMultipartAbort(t *testing.T) {
	s, mb := newMockBackend(t)
	up, err := s.CreateMultipart(context.Background(), "k", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := up.Abort(context.Background()); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if _, ok := mb.uploads[up.UploadID()]; ok {
		t.Fatalf("upload still present after abort")
	}
}

func TestCreateMultipartRejectsInvalidKey(t *testing.T) {
	s, _ := newMockBackend(t)
	bad := []string{"", "/foo", "foo/", "foo\x00bar"}
	for _, k := range bad {
		t.Run(k, func(t *testing.T) {
			_, err := s.CreateMultipart(context.Background(), k, nil)
			if !errors.Is(err, storage.ErrInvalidArgument) {
				t.Fatalf("CreateMultipart(%q) err = %v, want ErrInvalidArgument", k, err)
			}
		})
	}
}
