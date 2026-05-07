package s3compat

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
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

func TestUploadPartReportsCorrectSize(t *testing.T) {
	s, _ := newMockBackend(t)
	up, err := s.CreateMultipart(context.Background(), "k", nil)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.NewReader("hello world!!")
	p, err := up.UploadPart(context.Background(), 1, body)
	if err != nil {
		t.Fatalf("UploadPart: %v", err)
	}
	if p.Size != int64(len("hello world!!")) {
		t.Fatalf("Size = %d, want %d", p.Size, len("hello world!!"))
	}
}

func TestCompleteMultipartIfAbsentRejectsEmptyParts(t *testing.T) {
	s, _ := newMockBackend(t)
	up, err := s.CreateMultipart(context.Background(), "k", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CompleteMultipartIfAbsent(context.Background(), up, nil)
	if !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("err = %v, want ErrInvalidArgument (empty parts)", err)
	}
}

func TestCompleteMultipartIfAbsentRejectsNonContiguousParts(t *testing.T) {
	s, _ := newMockBackend(t)
	up, err := s.CreateMultipart(context.Background(), "k", nil)
	if err != nil {
		t.Fatal(err)
	}
	p1, _ := up.UploadPart(context.Background(), 1, strings.NewReader("a"))
	// Upload part 3 (skipping 2) — caller error
	p3, _ := up.UploadPart(context.Background(), 3, strings.NewReader("c"))
	_, err = s.CompleteMultipartIfAbsent(context.Background(), up,
		[]storage.MultipartPart{p1, p3})
	if !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("err = %v, want ErrInvalidArgument (non-contiguous)", err)
	}
}

func TestCompleteMultipartIfAbsentRejectsTamperedSize(t *testing.T) {
	s, _ := newMockBackend(t)
	up, err := s.CreateMultipart(context.Background(), "k", nil)
	if err != nil {
		t.Fatal(err)
	}
	p1, _ := up.UploadPart(context.Background(), 1, strings.NewReader("hi"))
	p1.Size = 99999 // caller tampered
	_, err = s.CompleteMultipartIfAbsent(context.Background(), up, []storage.MultipartPart{p1})
	if !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("err = %v, want ErrInvalidArgument (tampered size)", err)
	}
}

func TestAbortIsIdempotent(t *testing.T) {
	s, _ := newMockBackend(t)
	up, err := s.CreateMultipart(context.Background(), "k", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := up.Abort(context.Background()); err != nil {
		t.Fatalf("first Abort: %v", err)
	}
	// Second abort: upload no longer exists. Must succeed.
	if err := up.Abort(context.Background()); err != nil {
		t.Fatalf("second Abort: %v (must be idempotent)", err)
	}
}

func TestCompleteMultipartRejectsCrossInstance(t *testing.T) {
	s1, _ := newMockBackend(t)
	s2, _ := newMockBackend(t)
	up, err := s1.CreateMultipart(context.Background(), "k", nil)
	if err != nil {
		t.Fatal(err)
	}
	p1, _ := up.UploadPart(context.Background(), 1, strings.NewReader("hi"))
	// Try to complete on the WRONG S3Compat instance.
	_, err = s2.CompleteMultipartIfAbsent(context.Background(), up,
		[]storage.MultipartPart{p1})
	if !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("err = %v, want ErrInvalidArgument (cross-instance)", err)
	}
}

func TestUploadPartRejectsInvalidPartNumber(t *testing.T) {
	s, _ := newMockBackend(t)
	up, err := s.CreateMultipart(context.Background(), "k", nil)
	if err != nil {
		t.Fatal(err)
	}
	bad := []int{0, -1, 10001, 1 << 30}
	for _, pn := range bad {
		_, err := up.UploadPart(context.Background(), pn, strings.NewReader("x"))
		if !errors.Is(err, storage.ErrInvalidArgument) {
			t.Fatalf("UploadPart(pn=%d): err = %v, want ErrInvalidArgument", pn, err)
		}
	}
}

func TestCompleteAfterCompleteRejects(t *testing.T) {
	s, _ := newMockBackend(t)
	up, err := s.CreateMultipart(context.Background(), "k", nil)
	if err != nil {
		t.Fatal(err)
	}
	p1, _ := up.UploadPart(context.Background(), 1, strings.NewReader("hi"))
	if _, err := s.CompleteMultipartIfAbsent(context.Background(), up,
		[]storage.MultipartPart{p1}); err != nil {
		t.Fatalf("first Complete: %v", err)
	}
	// Second Complete on the same upload should fail fast as ErrInvalidArgument.
	_, err = s.CompleteMultipartIfAbsent(context.Background(), up,
		[]storage.MultipartPart{p1})
	if !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("second Complete: err = %v, want ErrInvalidArgument (terminated)", err)
	}
}

func TestUploadPartAfterAbortRejects(t *testing.T) {
	s, _ := newMockBackend(t)
	up, err := s.CreateMultipart(context.Background(), "k", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := up.Abort(context.Background()); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	_, err = up.UploadPart(context.Background(), 1, strings.NewReader("x"))
	if !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("UploadPart after Abort: err = %v, want ErrInvalidArgument", err)
	}
}

func TestCompleteAfterAbortRejects(t *testing.T) {
	s, _ := newMockBackend(t)
	up, err := s.CreateMultipart(context.Background(), "k", nil)
	if err != nil {
		t.Fatal(err)
	}
	p1, _ := up.UploadPart(context.Background(), 1, strings.NewReader("hi"))
	if err := up.Abort(context.Background()); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	_, err = s.CompleteMultipartIfAbsent(context.Background(), up,
		[]storage.MultipartPart{p1})
	if !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("Complete after Abort: err = %v, want ErrInvalidArgument", err)
	}
}

func TestConcurrentCompleteSerializes(t *testing.T) {
	s, _ := newMockBackend(t)
	up, err := s.CreateMultipart(context.Background(), "k", nil)
	if err != nil {
		t.Fatal(err)
	}
	p1, _ := up.UploadPart(context.Background(), 1, strings.NewReader("hi"))

	// Two goroutines race to Complete.
	var wg sync.WaitGroup
	wg.Add(2)
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			_, err := s.CompleteMultipartIfAbsent(context.Background(), up,
				[]storage.MultipartPart{p1})
			results <- err
		}()
	}
	wg.Wait()
	close(results)

	// Exactly one should succeed; the other should report
	// ErrInvalidArgument (terminated state), NOT ErrNotFound (which
	// would be a leak of provider lifecycle state).
	var successes, terminated int
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, storage.ErrInvalidArgument):
			terminated++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("successes = %d, want 1", successes)
	}
	if terminated != 1 {
		t.Fatalf("terminated = %d, want 1 (loser must report ErrInvalidArgument, not pass through to NoSuchUpload)", terminated)
	}
}
