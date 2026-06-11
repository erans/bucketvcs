package conformance

import (
	"bytes"
	"errors"
	"io"
	"sort"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// runCorrectness is the entry point for the §29 correctness tests. Each
// test corresponds to one numbered item in §29.
func runCorrectness(t *testing.T, f Factory) {
	t.Helper()
	t.Run("Name", func(t *testing.T) { testName(t, f) })
	t.Run("§29#4_PutThenGet_RAW", func(t *testing.T) { test29_4(t, f) })
	t.Run("§29#9_GetRange", func(t *testing.T) { test29_9(t, f) })
	t.Run("§29#1_ConcurrentPutIfAbsent", func(t *testing.T) { test29_1(t, f) })
	t.Run("§29#14_PutIfAbsentIdempotentRetry", func(t *testing.T) { test29_14(t, f) })
	t.Run("§29#2_ConcurrentPutIfVersionMatches", func(t *testing.T) { test29_2(t, f) })
	t.Run("§29#3_FailedConditionalDoesNotAlter", func(t *testing.T) { test29_3(t, f) })
	t.Run("§29#5_OverwriteThenRead", func(t *testing.T) { test29_5(t, f) })
	t.Run("§29#11_DeleteIfVersionMatches", func(t *testing.T) { test29_11(t, f) })
	t.Run("§29#6_ListAfterWrite", func(t *testing.T) { test29_6(t, f) })
	t.Run("§29#7_ListAfterDelete", func(t *testing.T) { test29_7(t, f) })
	t.Run("ListPagination", func(t *testing.T) { testListPagination(t, f) })
	t.Run("ListDelimiter", func(t *testing.T) { testListDelimiter(t, f) })
	t.Run("MultipartHappyPath", func(t *testing.T) { testMultipartHappyPath(t, f) })
	t.Run("§29#8_MultipartCannotOverwrite", func(t *testing.T) { test29_8(t, f) })
	t.Run("§29#10_SignedURL", func(t *testing.T) { test29_10(t, f) })
	t.Run("§29#12_VersionRoundTrip", func(t *testing.T) { test29_12(t, f) })
	t.Run("§29#13_CASConflictClassification", func(t *testing.T) { test29_13(t, f) })
	t.Run("§29#15_ThrottlingClassification", func(t *testing.T) { test29_15(t, f) })
	t.Run("KeyNamespace", func(t *testing.T) { testKeyNamespace(t, f) })
	t.Run("MultipartInvalidPartNumber", func(t *testing.T) { testMultipartInvalidPartNumber(t, f) })
	t.Run("MultipartRepeatedPartNumber", func(t *testing.T) { testMultipartRepeatedPartNumber(t, f) })
	t.Run("MultipartCompleteEmptyParts", func(t *testing.T) { testMultipartCompleteEmptyParts(t, f) })
	t.Run("MultipartCompleteNonContiguous", func(t *testing.T) { testMultipartCompleteNonContiguous(t, f) })
	t.Run("MultipartCompleteSizeMismatch", func(t *testing.T) { testMultipartCompleteSizeMismatch(t, f) })
	t.Run("MultipartConcurrentComplete", func(t *testing.T) { testMultipartConcurrentComplete(t, f) })
	t.Run("MultipartAbortIdempotent", func(t *testing.T) { testMultipartAbortIdempotent(t, f) })
	t.Run("MultipartCompleteAfterAbort", func(t *testing.T) { testMultipartCompleteAfterAbort(t, f) })
}

// testName asserts the backend returns a non-empty kind identifier from
// ObjectStore.Name(). Used by receivepack to populate
// PushPayload.StorageBackend (spec §24).
func testName(t *testing.T, f Factory) {
	s := newStore(t, f)
	n := s.Name()
	if n == "" {
		t.Errorf("Name() returned empty string; want a kind identifier")
		return
	}
	for _, r := range n {
		switch {
		case r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			// ok
		default:
			t.Errorf("Name() = %q; expected lowercase alphanumeric / -_.", n)
			return
		}
	}
}

// §29 #4: Read after write sees latest object.
func test29_4(t *testing.T, f Factory) {
	s := newStore(t, f)
	want := []byte("hello world")
	v, err := s.PutIfAbsent(ctx(), "rk/29-4", bytes.NewReader(want), nil)
	if err != nil {
		t.Fatalf("PutIfAbsent: %v", err)
	}
	if v.Token == "" {
		t.Fatal("PutIfAbsent returned empty version token")
	}

	md, err := s.Head(ctx(), "rk/29-4")
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if md.Size != int64(len(want)) {
		t.Errorf("Head Size = %d, want %d", md.Size, len(want))
	}
	if md.Version != v {
		t.Errorf("Head Version = %+v, want %+v", md.Version, v)
	}

	obj, err := s.Get(ctx(), "rk/29-4", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer obj.Body.Close()
	got, err := io.ReadAll(obj.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Get content = %q, want %q", got, want)
	}

	if _, err := s.Get(ctx(), "rk/missing", nil); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("Get(missing) = %v, want ErrNotFound", err)
	}
}

// §29 #9: Range read returns exact bytes (and truncates to EOF when
// endInclusive exceeds the object size, mirroring HTTP semantics).
func test29_9(t *testing.T, f Factory) {
	s := newStore(t, f)
	const size = 1 << 20 // 1 MiB
	content := DeterministicBytes(size, "29-9")
	if _, err := s.PutIfAbsent(ctx(), "rk/29-9", bytes.NewReader(content), nil); err != nil {
		t.Fatalf("PutIfAbsent: %v", err)
	}

	cases := []struct {
		start, end int64
	}{
		{0, 0},
		{0, 1023},
		{1024, 2047},
		{int64(size) - 1, int64(size) - 1},
		{int64(size) - 1024, int64(size) - 1},
	}
	for _, c := range cases {
		rc, err := s.GetRange(ctx(), "rk/29-9", c.start, c.end)
		if err != nil {
			t.Fatalf("GetRange[%d,%d]: %v", c.start, c.end, err)
		}
		got, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("ReadAll[%d,%d]: %v", c.start, c.end, err)
		}
		want := content[c.start : c.end+1]
		if !bytes.Equal(got, want) {
			t.Errorf("GetRange[%d,%d] mismatch: got len=%d want len=%d", c.start, c.end, len(got), len(want))
		}
	}

	// Off-end: end exceeds content size; expect truncation to EOF.
	rc, err := s.GetRange(ctx(), "rk/29-9", int64(size-10), int64(size+1000))
	if err != nil {
		t.Fatalf("GetRange off-end: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if len(got) != 10 {
		t.Errorf("GetRange off-end returned %d bytes, want 10", len(got))
	}

	// Invalid: negative start.
	if _, err := s.GetRange(ctx(), "rk/29-9", -1, 5); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("GetRange(negative start) = %v, want ErrInvalidArgument", err)
	}

	// Missing key.
	if _, err := s.GetRange(ctx(), "rk/29-9-missing", 0, 0); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("GetRange(missing) = %v, want ErrNotFound", err)
	}
}

// §29 #1: Concurrent putIfAbsent same key — exactly one succeeds.
func test29_1(t *testing.T, f Factory) {
	s := newStore(t, f)
	const n = 64
	content := []byte("payload-29-1")
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := s.PutIfAbsent(ctx(), "rk/29-1", bytes.NewReader(content), nil)
			results <- err
		}()
	}
	successes, conflicts, others := 0, 0, 0
	for i := 0; i < n; i++ {
		err := <-results
		switch {
		case err == nil:
			successes++
		case errors.Is(err, storage.ErrAlreadyExists):
			conflicts++
		default:
			others++
			t.Errorf("unexpected error: %v", err)
		}
	}
	if successes != 1 {
		t.Errorf("successes = %d, want 1", successes)
	}
	if conflicts != n-1 {
		t.Errorf("conflicts = %d, want %d", conflicts, n-1)
	}
}

// §29 #14 (recast per AD8): PutIfAbsent twice with the same args returns
// ErrAlreadyExists cleanly without corrupting state on the second call.
// See M0 design doc Architectural Decision 8.
func test29_14(t *testing.T, f Factory) {
	s := newStore(t, f)
	content := []byte("payload-29-14")
	v1, err := s.PutIfAbsent(ctx(), "rk/29-14", bytes.NewReader(content), nil)
	if err != nil {
		t.Fatalf("first PutIfAbsent: %v", err)
	}
	if _, err := s.PutIfAbsent(ctx(), "rk/29-14", bytes.NewReader(content), nil); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Errorf("second PutIfAbsent = %v, want ErrAlreadyExists", err)
	}

	md, err := s.Head(ctx(), "rk/29-14")
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if md.Version != v1 {
		t.Errorf("version mutated by failed second PutIfAbsent: got %+v, want %+v", md.Version, v1)
	}
}

// §29 #2: Concurrent putIfVersionMatches same key — exactly one succeeds.
func test29_2(t *testing.T, f Factory) {
	s := newStore(t, f)
	v0, err := s.PutIfAbsent(ctx(), "rk/29-2", bytes.NewReader([]byte("v0")), nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	const n = 64
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := s.PutIfVersionMatches(ctx(), "rk/29-2", v0, bytes.NewReader([]byte("v1")), nil)
			results <- err
		}()
	}
	successes, conflicts, others := 0, 0, 0
	for i := 0; i < n; i++ {
		err := <-results
		switch {
		case err == nil:
			successes++
		case errors.Is(err, storage.ErrVersionMismatch):
			conflicts++
		default:
			others++
			t.Errorf("unexpected error: %v", err)
		}
	}
	if successes != 1 {
		t.Errorf("successes = %d, want 1", successes)
	}
	if conflicts != n-1 {
		t.Errorf("conflicts = %d, want %d", conflicts, n-1)
	}
}

// §29 #3: Failed conditional write does not alter object.
func test29_3(t *testing.T, f Factory) {
	s := newStore(t, f)
	want := []byte("original")
	v0, err := s.PutIfAbsent(ctx(), "rk/29-3", bytes.NewReader(want), nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	bogus := storage.ObjectVersion{Provider: v0.Provider, Token: "deadbeef", Kind: v0.Kind}
	if _, err := s.PutIfVersionMatches(ctx(), "rk/29-3", bogus, bytes.NewReader([]byte("DROP")), nil); !errors.Is(err, storage.ErrVersionMismatch) {
		t.Errorf("PutIfVersionMatches(bogus) = %v, want ErrVersionMismatch", err)
	}

	obj, err := s.Get(ctx(), "rk/29-3", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer obj.Body.Close()
	got, _ := io.ReadAll(obj.Body)
	if !bytes.Equal(got, want) {
		t.Errorf("content mutated by failed conditional: got %q, want %q", got, want)
	}
	if obj.Metadata.Version != v0 {
		t.Errorf("version mutated by failed conditional: got %+v, want %+v", obj.Metadata.Version, v0)
	}
}

// §29 #5: Read after overwrite sees the latest object.
func test29_5(t *testing.T, f Factory) {
	s := newStore(t, f)
	v0, err := s.PutIfAbsent(ctx(), "rk/29-5", bytes.NewReader([]byte("v0")), nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	v1, err := s.PutIfVersionMatches(ctx(), "rk/29-5", v0, bytes.NewReader([]byte("v1-content")), nil)
	if err != nil {
		t.Fatalf("PutIfVersionMatches: %v", err)
	}
	if v1 == v0 {
		t.Error("version did not change after overwrite")
	}
	obj, err := s.Get(ctx(), "rk/29-5", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer obj.Body.Close()
	got, _ := io.ReadAll(obj.Body)
	if string(got) != "v1-content" {
		t.Errorf("after overwrite content = %q, want %q", got, "v1-content")
	}
	if obj.Metadata.Version != v1 {
		t.Errorf("Metadata.Version = %+v, want %+v", obj.Metadata.Version, v1)
	}
}

// §29 #11: DeleteIfVersionMatches fails if object changed.
func test29_11(t *testing.T, f Factory) {
	s := newStore(t, f)
	v0, err := s.PutIfAbsent(ctx(), "rk/29-11", bytes.NewReader([]byte("v0")), nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	v1, err := s.PutIfVersionMatches(ctx(), "rk/29-11", v0, bytes.NewReader([]byte("v1")), nil)
	if err != nil {
		t.Fatalf("overwrite: %v", err)
	}

	if err := s.DeleteIfVersionMatches(ctx(), "rk/29-11", v0); !errors.Is(err, storage.ErrVersionMismatch) {
		t.Errorf("DeleteIfVersionMatches(stale) = %v, want ErrVersionMismatch", err)
	}
	if _, err := s.Head(ctx(), "rk/29-11"); err != nil {
		t.Errorf("after failed delete, Head = %v, want nil", err)
	}

	if err := s.DeleteIfVersionMatches(ctx(), "rk/29-11", v1); err != nil {
		t.Errorf("DeleteIfVersionMatches(current) = %v, want nil", err)
	}
	if _, err := s.Head(ctx(), "rk/29-11"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("after delete, Head = %v, want ErrNotFound", err)
	}
	if err := s.DeleteIfVersionMatches(ctx(), "rk/29-11", v1); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("DeleteIfVersionMatches(absent) = %v, want ErrNotFound", err)
	}
}

// §29 #6: List after write sees new object.
func test29_6(t *testing.T, f Factory) {
	s := newStore(t, f)
	for i := 0; i < 5; i++ {
		key := Key("p/29-6", i)
		if _, err := s.PutIfAbsent(ctx(), key, bytes.NewReader([]byte("x")), nil); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
	page, err := s.List(ctx(), "p/29-6/", &storage.ListOptions{MaxKeys: 100})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Objects) != 5 {
		t.Errorf("listed %d objects, want 5", len(page.Objects))
	}
}

// §29 #7: List after delete does not show deleted object.
func test29_7(t *testing.T, f Factory) {
	s := newStore(t, f)
	v, err := s.PutIfAbsent(ctx(), "p/29-7/a", bytes.NewReader([]byte("a")), nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.DeleteIfVersionMatches(ctx(), "p/29-7/a", v); err != nil {
		t.Fatalf("delete: %v", err)
	}
	page, err := s.List(ctx(), "p/29-7/", nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, md := range page.Objects {
		if md.Key == "p/29-7/a" {
			t.Error("listed deleted object")
		}
	}
}

// Pagination: List returns at most MaxKeys; subsequent calls with
// NextToken cover the remainder; concatenation matches the full set.
func testListPagination(t *testing.T, f Factory) {
	s := newStore(t, f)
	const total = 25
	for i := 0; i < total; i++ {
		if _, err := s.PutIfAbsent(ctx(), Key("p/page", i), bytes.NewReader([]byte{byte(i)}), nil); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	got := map[string]bool{}
	var ordered []string
	token := ""
	for iter := 0; iter < 100; iter++ {
		page, err := s.List(ctx(), "p/page/", &storage.ListOptions{MaxKeys: 7, ContinuationToken: token})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, md := range page.Objects {
			if got[md.Key] {
				t.Errorf("duplicate key in pagination: %s", md.Key)
			}
			got[md.Key] = true
			ordered = append(ordered, md.Key)
		}
		if page.NextToken == "" {
			break
		}
		if len(page.Objects) > 7 {
			t.Errorf("page returned %d objects, want <= 7", len(page.Objects))
		}
		token = page.NextToken
	}
	if len(got) != total {
		t.Errorf("paginated total = %d, want %d", len(got), total)
	}
	// Ordering contract: List returns keys lexicographically ascending,
	// within a page AND across pages. internal/auditlog's day-walk floor
	// probe (List MaxKeys:1 -> oldest key) depends on this.
	if !sort.StringsAreSorted(ordered) {
		t.Errorf("List keys not lexicographically ascending across pages: %v", ordered)
	}
}

func testListDelimiter(t *testing.T, f Factory) {
	s := newStore(t, f)
	keys := []string{
		"d/a/1",
		"d/a/2",
		"d/b/1",
		"d/c",
	}
	for _, k := range keys {
		if _, err := s.PutIfAbsent(ctx(), k, bytes.NewReader([]byte("x")), nil); err != nil {
			t.Fatalf("seed %s: %v", k, err)
		}
	}

	page, err := s.List(ctx(), "d/", &storage.ListOptions{Delimiter: "/", MaxKeys: 100})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	wantPrefixes := map[string]bool{"d/a/": true, "d/b/": true}
	gotPrefixes := map[string]bool{}
	for _, p := range page.CommonPrefixes {
		gotPrefixes[p] = true
	}
	for p := range wantPrefixes {
		if !gotPrefixes[p] {
			t.Errorf("missing common prefix %q (got %v)", p, page.CommonPrefixes)
		}
	}
	wantObjs := map[string]bool{"d/c": true}
	gotObjs := map[string]bool{}
	for _, md := range page.Objects {
		gotObjs[md.Key] = true
	}
	for k := range wantObjs {
		if !gotObjs[k] {
			t.Errorf("missing direct object %q (got %v)", k, page.Objects)
		}
	}
}

func testMultipartHappyPath(t *testing.T, f Factory) {
	s := newStore(t, f)
	// S3 (and MinIO) enforce a 5 MiB minimum per part for all parts except the
	// last.  Use 5 MiB+1 so all parts satisfy the minimum regardless of
	// position; keep numParts small to limit test data volume.
	const partSize = 5<<20 + 1 // 5 MiB + 1 byte
	const numParts = 2
	full := DeterministicBytes(partSize*numParts, "multi-happy")

	mp, err := s.CreateMultipart(ctx(), "rk/multi-happy", &storage.MultipartOptions{ContentType: "application/octet-stream"})
	if err != nil {
		t.Fatalf("CreateMultipart: %v", err)
	}
	if mp.UploadID() == "" {
		t.Error("UploadID empty")
	}
	if mp.Key() != "rk/multi-happy" {
		t.Errorf("Key = %q, want %q", mp.Key(), "rk/multi-happy")
	}

	parts := make([]storage.MultipartPart, 0, numParts)
	for i := 0; i < numParts; i++ {
		chunk := full[i*partSize : (i+1)*partSize]
		p, err := mp.UploadPart(ctx(), i+1, bytes.NewReader(chunk))
		if err != nil {
			t.Fatalf("UploadPart %d: %v", i+1, err)
		}
		parts = append(parts, p)
	}

	v, err := s.CompleteMultipartIfAbsent(ctx(), mp, parts)
	if err != nil {
		t.Fatalf("CompleteMultipartIfAbsent: %v", err)
	}
	if v.Token == "" {
		t.Error("complete returned empty version token")
	}

	obj, err := s.Get(ctx(), "rk/multi-happy", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer obj.Body.Close()
	got, _ := io.ReadAll(obj.Body)
	if !bytes.Equal(got, full) {
		t.Errorf("multipart content mismatch: got len=%d want len=%d", len(got), len(full))
	}
}

// §29 #8: Multipart complete cannot silently overwrite existing object.
func test29_8(t *testing.T, f Factory) {
	s := newStore(t, f)
	original := []byte("original")
	v0, err := s.PutIfAbsent(ctx(), "rk/29-8", bytes.NewReader(original), nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	mp, err := s.CreateMultipart(ctx(), "rk/29-8", nil)
	if err != nil {
		t.Fatalf("CreateMultipart: %v", err)
	}
	p, err := mp.UploadPart(ctx(), 1, bytes.NewReader([]byte("DROP")))
	if err != nil {
		t.Fatalf("UploadPart: %v", err)
	}
	if _, err := s.CompleteMultipartIfAbsent(ctx(), mp, []storage.MultipartPart{p}); !errors.Is(err, storage.ErrAlreadyExists) {
		t.Errorf("Complete on existing key = %v, want ErrAlreadyExists", err)
	}

	obj, err := s.Get(ctx(), "rk/29-8", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer obj.Body.Close()
	got, _ := io.ReadAll(obj.Body)
	if !bytes.Equal(got, original) {
		t.Errorf("original mutated: got %q, want %q", got, original)
	}
	if obj.Metadata.Version != v0 {
		t.Errorf("version mutated: got %+v, want %+v", obj.Metadata.Version, v0)
	}
}

// §29 #10: Signed URL can read but cannot write. Adapters that do not
// support signed URLs declare so via Capabilities and return
// ErrNotSupported from SignedGetURL.
func test29_10(t *testing.T, f Factory) {
	s := newStore(t, f)
	caps := s.Capabilities()
	if !caps.SignedURLs {
		_, _, err := s.SignedGetURL(ctx(), "rk/29-10", storage.SignedURLOptions{Expires: 0, Method: "GET"})
		if !errors.Is(err, storage.ErrNotSupported) {
			t.Errorf("Capabilities.SignedURLs=false but SignedGetURL = %v, want ErrNotSupported", err)
		}
		return
	}
	t.Skip("adapter declares SignedURLs=true; full URL semantics tested by adapter-specific suite")
}

// §29 #12: Metadata/version token round-trips Put → Head.
func test29_12(t *testing.T, f Factory) {
	s := newStore(t, f)
	v, err := s.PutIfAbsent(ctx(), "rk/29-12", bytes.NewReader([]byte("payload")), &storage.PutOptions{ContentType: "text/plain"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	md, err := s.Head(ctx(), "rk/29-12")
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if md.Version != v {
		t.Errorf("Head Version = %+v, want %+v", md.Version, v)
	}
	if md.Size != int64(len("payload")) {
		t.Errorf("Size = %d, want %d", md.Size, len("payload"))
	}
	if md.ContentType != "text/plain" {
		t.Errorf("ContentType = %q, want %q", md.ContentType, "text/plain")
	}
	if md.Key != "rk/29-12" {
		t.Errorf("Key = %q, want %q", md.Key, "rk/29-12")
	}
}

// §29 #13: CAS conflict error maps to normalized conflict type.
func test29_13(t *testing.T, f Factory) {
	s := newStore(t, f)
	if _, err := s.PutIfAbsent(ctx(), "rk/29-13", bytes.NewReader([]byte("a")), nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := s.PutIfAbsent(ctx(), "rk/29-13", bytes.NewReader([]byte("b")), nil)
	if !errors.Is(err, storage.ErrAlreadyExists) {
		t.Errorf("PutIfAbsent on existing = %v, want ErrAlreadyExists", err)
	}

	_, err = s.PutIfVersionMatches(ctx(), "rk/29-13", storage.ObjectVersion{Token: "deadbeef"}, bytes.NewReader([]byte("c")), nil)
	if !errors.Is(err, storage.ErrVersionMismatch) {
		t.Errorf("PutIfVersionMatches stale = %v, want ErrVersionMismatch", err)
	}
}

// §29 #15: Provider throttling errors are classified correctly.
// Localfs does not throttle. Cloud adapters override this skip.
func test29_15(t *testing.T, f Factory) {
	t.Skip("localfs has no throttling; cloud adapters at M5/M7 inject and assert ErrThrottled")
}

// Key namespace floor: invalid keys must be rejected with
// ErrInvalidArgument across the contract.
func testKeyNamespace(t *testing.T, f Factory) {
	s := newStore(t, f)
	invalid := []string{
		"",
		"/leading-slash",
		"trailing-slash/",
		"contains/../segment",
		"with\x00null",
		"with\\backslash",
	}
	for _, k := range invalid {
		if _, err := s.PutIfAbsent(ctx(), k, bytes.NewReader([]byte("x")), nil); !errors.Is(err, storage.ErrInvalidArgument) {
			t.Errorf("PutIfAbsent(%q) = %v, want ErrInvalidArgument", k, err)
		}
		if _, err := s.Head(ctx(), k); !errors.Is(err, storage.ErrInvalidArgument) {
			t.Errorf("Head(%q) = %v, want ErrInvalidArgument", k, err)
		}
		if err := s.DeleteIfVersionMatches(ctx(), k, storage.ObjectVersion{}); !errors.Is(err, storage.ErrInvalidArgument) {
			t.Errorf("DeleteIfVersionMatches(%q) = %v, want ErrInvalidArgument", k, err)
		}
	}

	// Valid keys at the floor should succeed. Namespaces are chosen to
	// avoid file/directory collisions on adapters whose on-disk layout
	// maps key segments directly to filesystem paths (e.g., localfs):
	// a key that is a strict prefix of another would require the same
	// path to be both a file and a directory.
	for _, k := range []string{"alpha", "ns/leaf", "tenants/t1/repos/r1/manifest/root.json"} {
		if _, err := s.PutIfAbsent(ctx(), k, bytes.NewReader([]byte("x")), nil); err != nil {
			t.Errorf("PutIfAbsent(%q) returned %v, want nil", k, err)
		}
	}
}

// MultipartInvalidPartNumber: UploadPart with partNumber < 1 returns
// ErrInvalidArgument.
func testMultipartInvalidPartNumber(t *testing.T, f Factory) {
	s := newStore(t, f)
	mp, err := s.CreateMultipart(ctx(), "rk/multi-invalid", nil)
	if err != nil {
		t.Fatalf("CreateMultipart: %v", err)
	}
	if _, err := mp.UploadPart(ctx(), 0, bytes.NewReader([]byte("x"))); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("UploadPart(0) = %v, want ErrInvalidArgument", err)
	}
	if _, err := mp.UploadPart(ctx(), -1, bytes.NewReader([]byte("x"))); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("UploadPart(-1) = %v, want ErrInvalidArgument", err)
	}
	_ = mp.Abort(ctx())
}

// MultipartRepeatedPartNumber: uploading the same partNumber twice
// succeeds; Complete uses the second upload's bytes.
func testMultipartRepeatedPartNumber(t *testing.T, f Factory) {
	s := newStore(t, f)
	mp, err := s.CreateMultipart(ctx(), "rk/multi-repeat", nil)
	if err != nil {
		t.Fatalf("CreateMultipart: %v", err)
	}
	if _, err := mp.UploadPart(ctx(), 1, bytes.NewReader([]byte("first"))); err != nil {
		t.Fatalf("UploadPart 1 first: %v", err)
	}
	p1, err := mp.UploadPart(ctx(), 1, bytes.NewReader([]byte("SECOND")))
	if err != nil {
		t.Fatalf("UploadPart 1 second: %v", err)
	}
	if _, err := s.CompleteMultipartIfAbsent(ctx(), mp, []storage.MultipartPart{p1}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	obj, err := s.Get(ctx(), "rk/multi-repeat", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer obj.Body.Close()
	got, _ := io.ReadAll(obj.Body)
	if string(got) != "SECOND" {
		t.Errorf("content = %q, want %q (second upload should win)", got, "SECOND")
	}
}

// MultipartCompleteEmptyParts: Complete with empty parts slice returns
// ErrInvalidArgument.
func testMultipartCompleteEmptyParts(t *testing.T, f Factory) {
	s := newStore(t, f)
	mp, err := s.CreateMultipart(ctx(), "rk/multi-empty", nil)
	if err != nil {
		t.Fatalf("CreateMultipart: %v", err)
	}
	if _, err := s.CompleteMultipartIfAbsent(ctx(), mp, nil); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("Complete(nil parts) = %v, want ErrInvalidArgument", err)
	}
	if _, err := s.CompleteMultipartIfAbsent(ctx(), mp, []storage.MultipartPart{}); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("Complete(empty parts) = %v, want ErrInvalidArgument", err)
	}
	_ = mp.Abort(ctx())
}

// MultipartCompleteNonContiguous: Complete with non-contiguous part
// numbers returns ErrInvalidArgument.
func testMultipartCompleteNonContiguous(t *testing.T, f Factory) {
	s := newStore(t, f)
	mp, err := s.CreateMultipart(ctx(), "rk/multi-noncontig", nil)
	if err != nil {
		t.Fatalf("CreateMultipart: %v", err)
	}
	p1, _ := mp.UploadPart(ctx(), 1, bytes.NewReader([]byte("a")))
	p3, _ := mp.UploadPart(ctx(), 3, bytes.NewReader([]byte("c")))
	if _, err := s.CompleteMultipartIfAbsent(ctx(), mp, []storage.MultipartPart{p1, p3}); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("Complete([1,3]) = %v, want ErrInvalidArgument", err)
	}
	_ = mp.Abort(ctx())
}

// MultipartCompleteSizeMismatch: Complete with a parts entry whose Size
// differs from the on-disk part size returns ErrInvalidArgument.
func testMultipartCompleteSizeMismatch(t *testing.T, f Factory) {
	s := newStore(t, f)
	mp, err := s.CreateMultipart(ctx(), "rk/multi-sizemis", nil)
	if err != nil {
		t.Fatalf("CreateMultipart: %v", err)
	}
	p1, err := mp.UploadPart(ctx(), 1, bytes.NewReader([]byte("five.")))
	if err != nil {
		t.Fatalf("UploadPart: %v", err)
	}
	p1.Size = p1.Size + 1 // claim wrong size
	if _, err := s.CompleteMultipartIfAbsent(ctx(), mp, []storage.MultipartPart{p1}); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Errorf("Complete(size mismatch) = %v, want ErrInvalidArgument", err)
	}
	_ = mp.Abort(ctx())
}

// MultipartConcurrentComplete: two concurrent Complete calls on the
// same upload+target serialize via the per-key mutex; one wins, the
// other sees ErrAlreadyExists.
func testMultipartConcurrentComplete(t *testing.T, f Factory) {
	s := newStore(t, f)
	mp, err := s.CreateMultipart(ctx(), "rk/multi-conc", nil)
	if err != nil {
		t.Fatalf("CreateMultipart: %v", err)
	}
	p1, err := mp.UploadPart(ctx(), 1, bytes.NewReader([]byte("payload")))
	if err != nil {
		t.Fatalf("UploadPart: %v", err)
	}

	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := s.CompleteMultipartIfAbsent(ctx(), mp, []storage.MultipartPart{p1})
			results <- err
		}()
	}
	successes, conflicts, others := 0, 0, 0
	for i := 0; i < 2; i++ {
		err := <-results
		switch {
		case err == nil:
			successes++
		case errors.Is(err, storage.ErrAlreadyExists):
			conflicts++
		case errors.Is(err, storage.ErrInvalidArgument):
			// Acceptable: the second goroutine may observe the upload as
			// terminated (validateActive saw the in-memory flag or the
			// manifest removal that follows the first Complete) before
			// the per-key mutex contention had a chance to surface
			// ErrAlreadyExists. Either outcome rejects the second
			// completion without corrupting state, which is what the
			// test cares about.
			conflicts++
		default:
			others++
			t.Errorf("unexpected error: %v", err)
		}
	}
	if successes != 1 {
		t.Errorf("successes = %d, want 1", successes)
	}
	if conflicts != 1 {
		t.Errorf("conflicts = %d, want 1", conflicts)
	}
}

// MultipartAbortIdempotent: Abort after Complete is a no-op; Abort
// twice in a row is a no-op.
func testMultipartAbortIdempotent(t *testing.T, f Factory) {
	s := newStore(t, f)
	// Abort twice on a fresh upload.
	mp, err := s.CreateMultipart(ctx(), "rk/multi-abort1", nil)
	if err != nil {
		t.Fatalf("CreateMultipart: %v", err)
	}
	if err := mp.Abort(ctx()); err != nil {
		t.Errorf("Abort: %v", err)
	}
	if err := mp.Abort(ctx()); err != nil {
		t.Errorf("second Abort: %v", err)
	}

	// Abort after Complete.
	mp2, err := s.CreateMultipart(ctx(), "rk/multi-abort2", nil)
	if err != nil {
		t.Fatalf("CreateMultipart: %v", err)
	}
	p1, err := mp2.UploadPart(ctx(), 1, bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatalf("UploadPart: %v", err)
	}
	if _, err := s.CompleteMultipartIfAbsent(ctx(), mp2, []storage.MultipartPart{p1}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if err := mp2.Abort(ctx()); err != nil {
		t.Errorf("Abort after Complete: %v", err)
	}
}

// MultipartCompleteAfterAbort: Complete after Abort fails. The error
// surface is "Complete is not callable after Abort"; the precise sentinel
// is the adapter's choice.
func testMultipartCompleteAfterAbort(t *testing.T, f Factory) {
	s := newStore(t, f)
	mp, err := s.CreateMultipart(ctx(), "rk/multi-cafterA", nil)
	if err != nil {
		t.Fatalf("CreateMultipart: %v", err)
	}
	p1, err := mp.UploadPart(ctx(), 1, bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatalf("UploadPart: %v", err)
	}
	if err := mp.Abort(ctx()); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if _, err := s.CompleteMultipartIfAbsent(ctx(), mp, []storage.MultipartPart{p1}); err == nil {
		t.Error("Complete after Abort returned nil, want non-nil error")
	}
}
