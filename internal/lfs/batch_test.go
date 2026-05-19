package lfs

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// Valid LFS OIDs are 64 lowercase hex chars. The Build code path
// validates this before any store call, so tests must use real-shape
// OIDs to exercise the success branches. We pick per-purpose constants
// so test failures point to the right scenario; using the test name as
// a mnemonic suffix is not viable because non-hex chars are rejected.
const (
	oidNew      = "1111111111111111111111111111111111111111111111111111111111111111"
	oidExists   = "2222222222222222222222222222222222222222222222222222222222222222"
	oidMissing  = "3333333333333333333333333333333333333333333333333333333333333333"
	oidMismatch = "4444444444444444444444444444444444444444444444444444444444444444"
	oidPresent  = "5555555555555555555555555555555555555555555555555555555555555555"
	oidPresign  = "6666666666666666666666666666666666666666666666666666666666666666"
)

// fakeBatchStore is reused across batch tests. It exposes per-OID
// Head behavior and presign behavior so each table row can configure
// exactly the conditions Build branches on.
type fakeBatchStore struct {
	objects map[string]int64 // oid -> size (presence = exists)
	// signFn is called for both PresignPut and PresignGet; the test
	// inspects opts.Method to differentiate. Returning ErrNotSupported
	// exercises the proxied-fallback branch.
	signFn func(_ context.Context, key string, opts storage.SignedURLOptions) (string, error)
}

func (f *fakeBatchStore) Capabilities() storage.Capabilities {
	return storage.Capabilities{SignedURLs: true}
}
func (f *fakeBatchStore) Get(context.Context, string, *storage.GetOptions) (*storage.Object, error) {
	return nil, errors.New("not used")
}
func (f *fakeBatchStore) Head(_ context.Context, key string) (*storage.ObjectMetadata, error) {
	// key is "<prefix>/<oid>"; we look up by suffix after the last slash.
	oid := key
	if i := lastSlash(key); i >= 0 {
		oid = key[i+1:]
	}
	sz, ok := f.objects[oid]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return &storage.ObjectMetadata{Size: sz}, nil
}
func (f *fakeBatchStore) GetRange(context.Context, string, int64, int64) (io.ReadCloser, error) {
	return nil, errors.New("not used")
}
func (f *fakeBatchStore) PutIfAbsent(context.Context, string, io.Reader, *storage.PutOptions) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, errors.New("not used")
}
func (f *fakeBatchStore) PutIfVersionMatches(context.Context, string, storage.ObjectVersion, io.Reader, *storage.PutOptions) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, errors.New("not used")
}
func (f *fakeBatchStore) DeleteIfVersionMatches(context.Context, string, storage.ObjectVersion) error {
	return errors.New("not used")
}
func (f *fakeBatchStore) List(context.Context, string, *storage.ListOptions) (*storage.ListPage, error) {
	return nil, errors.New("not used")
}
func (f *fakeBatchStore) CreateMultipart(context.Context, string, *storage.MultipartOptions) (storage.MultipartUpload, error) {
	return nil, errors.New("not used")
}
func (f *fakeBatchStore) CompleteMultipartIfAbsent(context.Context, storage.MultipartUpload, []storage.MultipartPart) (storage.ObjectVersion, error) {
	return storage.ObjectVersion{}, errors.New("not used")
}
func (f *fakeBatchStore) SignedGetURL(ctx context.Context, key string, opts storage.SignedURLOptions) (string, error) {
	return f.signFn(ctx, key, opts)
}

func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}

func newBatchStore(objects map[string]int64, signFn func(context.Context, string, storage.SignedURLOptions) (string, error)) *Store {
	return NewStore(&fakeBatchStore{objects: objects, signFn: signFn}, "p/lfs/objects/")
}

// signedFn returns a sign function that returns a synthetic URL containing
// the method and key — enough for tests to assert routing without
// duplicating URL minting in the tests.
func signedFn() func(context.Context, string, storage.SignedURLOptions) (string, error) {
	return func(_ context.Context, key string, opts storage.SignedURLOptions) (string, error) {
		return "https://signed/" + opts.Method + "/" + key, nil
	}
}

// notSupportedFn forces Build into the proxied-fallback path. Today the
// proxied stub returns "" so Build surfaces a per-object 503.
func notSupportedFn() func(context.Context, string, storage.SignedURLOptions) (string, error) {
	return func(context.Context, string, storage.SignedURLOptions) (string, error) {
		return "", storage.ErrNotSupported
	}
}

func TestBuild_RejectsUnsupportedOperation(t *testing.T) {
	s := newBatchStore(nil, signedFn())
	_, err := Build(context.Background(), BatchRequest{Operation: "verify"}, s, "https://gw/verify", "Bearer x", time.Minute)
	if err == nil {
		t.Fatal("expected error for unsupported operation")
	}
}

func TestBuild_RejectsMissingBasicTransfer(t *testing.T) {
	s := newBatchStore(nil, signedFn())
	_, err := Build(context.Background(), BatchRequest{
		Operation: "upload",
		Transfers: []string{"lfs-standalone-file"}, // not "basic"
		Objects:   []ObjectRef{{OID: oidNew, Size: 1}},
	}, s, "https://gw/verify", "Bearer x", time.Minute)
	if err == nil {
		t.Fatal("expected error when basic transfer absent")
	}
}

func TestBuild_AcceptsImplicitBasicTransfer(t *testing.T) {
	// Per the LFS spec, omitting Transfers entirely is equivalent to ["basic"].
	s := newBatchStore(nil, signedFn())
	resp, err := Build(context.Background(), BatchRequest{
		Operation: "upload",
		Objects:   []ObjectRef{{OID: oidNew, Size: 1}},
	}, s, "https://gw/verify", "Bearer x", time.Minute)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Transfer != "basic" {
		t.Errorf("Transfer=%q", resp.Transfer)
	}
}

func TestBuild_Upload_NewObject(t *testing.T) {
	s := newBatchStore(nil, signedFn())
	resp, err := Build(context.Background(), BatchRequest{
		Operation: "upload",
		Transfers: []string{"basic"},
		Objects:   []ObjectRef{{OID: oidNew, Size: 100}},
	}, s, "https://gw/info/lfs/objects", "Bearer abc", time.Minute)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if resp.Transfer != "basic" || len(resp.Objects) != 1 {
		t.Fatalf("resp=%+v", resp)
	}
	o := resp.Objects[0]
	if o.OID != oidNew || o.Size != 100 || o.Error != nil {
		t.Errorf("o=%+v", o)
	}
	up, ok := o.Actions["upload"]
	if !ok {
		t.Fatal("upload action missing")
	}
	if up.Href != "https://signed/PUT/p/lfs/objects/"+oidNew {
		t.Errorf("upload Href=%q", up.Href)
	}
	if up.Header["Content-Type"] != "application/octet-stream" {
		t.Errorf("upload Content-Type=%q", up.Header["Content-Type"])
	}
	vf, ok := o.Actions["verify"]
	if !ok {
		t.Fatal("verify action missing")
	}
	if vf.Href != "https://gw/info/lfs/objects/"+oidNew+"/verify" {
		t.Errorf("verify Href=%q", vf.Href)
	}
	if vf.Header["Authorization"] != "Bearer abc" {
		t.Errorf("verify Authorization=%q", vf.Header["Authorization"])
	}
}

func TestBuild_Upload_ObjectAlreadyPresentAndSizeMatches(t *testing.T) {
	s := newBatchStore(map[string]int64{oidExists: 100}, signedFn())
	resp, err := Build(context.Background(), BatchRequest{
		Operation: "upload",
		Transfers: []string{"basic"},
		Objects:   []ObjectRef{{OID: oidExists, Size: 100}},
	}, s, "https://gw/verify", "Bearer abc", time.Minute)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	o := resp.Objects[0]
	if o.Error != nil {
		t.Errorf("unexpected error: %+v", o.Error)
	}
	if len(o.Actions) != 0 {
		t.Errorf("expected empty actions for existing object; got %+v", o.Actions)
	}
}

func TestBuild_Upload_ObjectPresentButSizeMismatch(t *testing.T) {
	s := newBatchStore(map[string]int64{oidMismatch: 50}, signedFn())
	resp, err := Build(context.Background(), BatchRequest{
		Operation: "upload",
		Transfers: []string{"basic"},
		Objects:   []ObjectRef{{OID: oidMismatch, Size: 100}},
	}, s, "https://gw/verify", "Bearer abc", time.Minute)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	o := resp.Objects[0]
	if o.Error == nil || o.Error.Code != 422 {
		t.Fatalf("expected per-object 422 error; got %+v", o.Error)
	}
	if len(o.Actions) != 0 {
		t.Errorf("expected no actions on size mismatch; got %+v", o.Actions)
	}
}

func TestBuild_Download_ObjectFound(t *testing.T) {
	s := newBatchStore(map[string]int64{oidExists: 200}, signedFn())
	resp, err := Build(context.Background(), BatchRequest{
		Operation: "download",
		Transfers: []string{"basic"},
		Objects:   []ObjectRef{{OID: oidExists, Size: 200}},
	}, s, "https://gw/verify", "Bearer abc", time.Minute)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	o := resp.Objects[0]
	if o.Error != nil {
		t.Errorf("unexpected error: %+v", o.Error)
	}
	dl, ok := o.Actions["download"]
	if !ok {
		t.Fatal("download action missing")
	}
	if dl.Href != "https://signed/GET/p/lfs/objects/"+oidExists {
		t.Errorf("download Href=%q", dl.Href)
	}
	if _, hasVerify := o.Actions["verify"]; hasVerify {
		t.Error("verify action must not appear on download")
	}
	if _, hasUpload := o.Actions["upload"]; hasUpload {
		t.Error("upload action must not appear on download")
	}
}

func TestBuild_Download_ObjectMissing(t *testing.T) {
	s := newBatchStore(nil, signedFn())
	resp, err := Build(context.Background(), BatchRequest{
		Operation: "download",
		Transfers: []string{"basic"},
		Objects:   []ObjectRef{{OID: oidMissing, Size: 100}},
	}, s, "https://gw/verify", "Bearer abc", time.Minute)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	o := resp.Objects[0]
	if o.Error == nil || o.Error.Code != 404 {
		t.Fatalf("expected per-object 404; got %+v", o.Error)
	}
	if len(o.Actions) != 0 {
		t.Errorf("expected no actions; got %+v", o.Actions)
	}
}

func TestBuild_PresignErrorBecomesPerObjectError(t *testing.T) {
	// PresignPut on a backend returning a real error (not ErrNotSupported)
	// must surface as a per-object error, not a top-level error: one bad
	// object should not poison the whole batch response.
	s := newBatchStore(nil, func(_ context.Context, _ string, _ storage.SignedURLOptions) (string, error) {
		return "", errors.New("presign failed")
	})
	resp, err := Build(context.Background(), BatchRequest{
		Operation: "upload",
		Transfers: []string{"basic"},
		Objects:   []ObjectRef{{OID: oidPresign, Size: 1}},
	}, s, "https://gw/verify", "Bearer abc", time.Minute)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	o := resp.Objects[0]
	if o.Error == nil || o.Error.Code != 500 {
		t.Fatalf("expected per-object 500; got %+v", o.Error)
	}
}

func TestBuild_ProxiedFallbackEmptyURLBecomesPerObject503(t *testing.T) {
	// When PresignPut returns ErrNotSupported AND ProxiedPutURL stub
	// returns "", Build must surface a per-object 503 so the LFS
	// client sees a clear failure instead of an empty Href.
	s := newBatchStore(nil, notSupportedFn())
	resp, err := Build(context.Background(), BatchRequest{
		Operation: "upload",
		Transfers: []string{"basic"},
		Objects:   []ObjectRef{{OID: oidPresign, Size: 1}},
	}, s, "https://gw/verify", "Bearer abc", time.Minute)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	o := resp.Objects[0]
	if o.Error == nil || o.Error.Code != 503 {
		t.Fatalf("expected per-object 503; got %+v", o.Error)
	}
}

func TestBuild_PerObjectIndependence(t *testing.T) {
	// Two objects: one exists with matching size (empty actions), one is
	// missing (upload+verify actions). Build must process them independently.
	s := newBatchStore(map[string]int64{oidPresent: 10}, signedFn())
	resp, err := Build(context.Background(), BatchRequest{
		Operation: "upload",
		Transfers: []string{"basic"},
		Objects: []ObjectRef{
			{OID: oidPresent, Size: 10},
			{OID: oidMissing, Size: 20},
		},
	}, s, "https://gw/verify", "Bearer abc", time.Minute)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(resp.Objects) != 2 {
		t.Fatalf("len(Objects)=%d", len(resp.Objects))
	}
	if len(resp.Objects[0].Actions) != 0 {
		t.Errorf("present object should have empty actions; got %+v", resp.Objects[0].Actions)
	}
	if _, ok := resp.Objects[1].Actions["upload"]; !ok {
		t.Error("missing object should have upload action")
	}
}

func TestBuild_Upload_RejectsNonPositiveSize(t *testing.T) {
	s := newBatchStore(nil, signedFn())
	for _, size := range []int64{0, -1} {
		resp, err := Build(context.Background(), BatchRequest{
			Operation: "upload",
			Transfers: []string{"basic"},
			Objects:   []ObjectRef{{OID: oidNew, Size: size}},
		}, s, "https://gw/verify", "Bearer abc", time.Minute)
		if err != nil {
			t.Fatalf("Build(size=%d): %v", size, err)
		}
		o := resp.Objects[0]
		if o.Error == nil || o.Error.Code != 422 {
			t.Errorf("size=%d: expected 422 per-object error; got %+v", size, o.Error)
		}
	}
}

func TestBuild_Download_RejectsNegativeSize(t *testing.T) {
	s := newBatchStore(map[string]int64{oidPresent: 100}, signedFn())
	resp, err := Build(context.Background(), BatchRequest{
		Operation: "download",
		Transfers: []string{"basic"},
		Objects:   []ObjectRef{{OID: oidPresent, Size: -1}},
	}, s, "https://gw/verify", "Bearer abc", time.Minute)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	o := resp.Objects[0]
	if o.Error == nil || o.Error.Code != 422 {
		t.Fatalf("expected per-object 422 for negative size; got %+v", o.Error)
	}
}

func TestBuild_Upload_OmitsEmptyVerifyAuthorization(t *testing.T) {
	s := newBatchStore(nil, signedFn())
	resp, err := Build(context.Background(), BatchRequest{
		Operation: "upload",
		Transfers: []string{"basic"},
		Objects:   []ObjectRef{{OID: oidNew, Size: 1}},
	}, s, "https://gw/verify", "", time.Minute) // empty bearer
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	vf, ok := resp.Objects[0].Actions["verify"]
	if !ok {
		t.Fatal("verify action missing")
	}
	if _, hasAuth := vf.Header["Authorization"]; hasAuth {
		t.Errorf("verify Header should omit Authorization when bearer is empty; got %+v", vf.Header)
	}
}

// TestBuild_RejectsInvalidOID covers the validOID guard in buildOne.
// Without this guard, a crafted OID like "../../other-tenant/file"
// would be concatenated into the storage key, escaping the per-repo
// prefix on localfs. Each case below should surface as a per-object
// 422 — the rest of the batch must remain processable.
func TestBuild_RejectsInvalidOID(t *testing.T) {
	s := newBatchStore(nil, signedFn())
	cases := []string{
		"",                            // empty
		"abc",                         // too short
		"ABCDEF" + strings.Repeat("0", 58), // uppercase
		"../escape",                   // path traversal
		strings.Repeat("1", 64) + "X", // too long
		strings.Repeat("1", 62) + "g1", // non-hex char in valid-length string
	}
	for _, oid := range cases {
		resp, err := Build(context.Background(), BatchRequest{
			Operation: "upload",
			Transfers: []string{"basic"},
			Objects:   []ObjectRef{{OID: oid, Size: 1}},
		}, s, "https://gw/verify", "Bearer abc", time.Minute)
		if err != nil {
			t.Fatalf("Build(oid=%q): %v", oid, err)
		}
		o := resp.Objects[0]
		if o.Error == nil || o.Error.Code != 422 {
			t.Errorf("oid=%q: expected per-object 422; got %+v", oid, o.Error)
		}
	}
}
