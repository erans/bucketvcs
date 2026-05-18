package conformance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// conformanceHTTPClient is used by mustHTTPGet/httpGetStatus to fetch
// signed URLs. A 30-second timeout prevents a stalled backend from
// blocking the whole test run.
var conformanceHTTPClient = &http.Client{Timeout: 30 * time.Second}

// RunCapabilitySigning verifies the positive path for SignedGetURL on
// adapters that advertise Capabilities().SignedURLs == true. The
// factory defensively skips when SignedURLs == false so it can be
// wired into a generic test harness; the M0 negative-path test
// independently covers the false-capability contract.
//
// Sub-tests:
//
//   - byte_identical_fetch: a fresh URL fetches a byte-identical copy.
//   - expired_signature_rejected: a 2-second URL returns 4xx after expiry.
//   - tampered_query_rejected: flipping bytes in the query returns 4xx.
//   - long_ttl_does_not_break_minting: a 1-year TTL either errors, is
//     silently clamped to a working URL, or is unconditionally issued.
//     All three outcomes satisfy the contract that excessive TTLs do
//     not produce a 5xx or transport-layer failure at mint time.
//   - expected_hash_binding: a URL minted with the correct sha256 fetches
//     successfully; a URL minted with a wrong sha256 mints without error
//     and the GET completes (with status 2xx if the adapter ignores the
//     field, 4xx if the adapter binds). Per-adapter tests assert the
//     stronger guarantee where applicable.
//   - signed_url_put_round_trip: a PUT URL minted via SignedGetURL with
//     Method="PUT" accepts an HTTP PUT, and a subsequent Head reports
//     the matching size.
func RunCapabilitySigning(t *testing.T, f Factory) {
	t.Helper()
	body := []byte("hello world")
	// Embed a body-hash fragment in the key so changing body in a future
	// commit automatically migrates to a new key; this prevents stale
	// objects in reused test buckets from masking byte_identical_fetch
	// failures.
	bodyHash := sha256.Sum256(body)
	key := "rk/m11-signing-" + hex.EncodeToString(bodyHash[:8])
	correctHash := "sha256:" + hex.EncodeToString(bodyHash[:])
	const wrongHash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

	// Capability gate: skip the whole factory once if the adapter does
	// not advertise SignedURLs. This avoids spinning up the factory
	// once per sub-test only to skip each time.
	{
		probe, cleanup := f(t)
		hasSigning := probe.Capabilities().SignedURLs
		cleanup()
		if !hasSigning {
			t.Skip("adapter does not advertise SignedURLs; skip RunCapabilitySigning")
		}
	}

	put := func(t *testing.T) (storage.ObjectStore, func()) {
		t.Helper()
		s, cleanup := f(t)
		if _, err := s.PutIfAbsent(context.Background(), key, bytes.NewReader(body), nil); err != nil {
			cleanup()
			t.Fatalf("PutIfAbsent: %v", err)
		}
		return s, cleanup
	}

	t.Run("byte_identical_fetch", func(t *testing.T) {
		s, cleanup := put(t)
		defer cleanup()
		raw, err := s.SignedGetURL(context.Background(), key, storage.SignedURLOptions{
			Expires: 30 * time.Second, Method: "GET",
		})
		if err != nil {
			t.Fatalf("SignedGetURL: %v", err)
		}
		got := mustHTTPGet(t, raw)
		if !bytes.Equal(got, body) {
			t.Fatalf("fetched bytes differ: got %q want %q", got, body)
		}
	})

	t.Run("expired_signature_rejected", func(t *testing.T) {
		s, cleanup := put(t)
		defer cleanup()
		raw, err := s.SignedGetURL(context.Background(), key, storage.SignedURLOptions{
			Expires: 2 * time.Second, Method: "GET",
		})
		if err != nil {
			t.Fatalf("SignedGetURL: %v", err)
		}
		time.Sleep(5 * time.Second)
		status := httpGetStatus(t, raw)
		switch {
		case status == -1:
			// Transport-layer failure also satisfies the contract: an
			// expired URL did not succeed.
		case status >= 400 && status < 500:
			// Server rejected the expired signature.
		default:
			t.Fatalf("expected 4xx or transport failure after expiry, got %d", status)
		}
	})

	t.Run("tampered_query_rejected", func(t *testing.T) {
		s, cleanup := put(t)
		defer cleanup()
		raw, err := s.SignedGetURL(context.Background(), key, storage.SignedURLOptions{
			Expires: 30 * time.Second, Method: "GET",
		})
		if err != nil {
			t.Fatalf("SignedGetURL: %v", err)
		}
		// Corrupt the signature value itself. S3 V4 uses "X-Amz-Signature=",
		// GCS V4 uses "X-Goog-Signature=", Azure SAS uses "sig=". Try each
		// in order; one must be present in any presigned URL we mint here.
		// The replacement byte is chosen to differ from whatever is at that
		// position (Azure SAS uses base64; an unconditional 'X' would be a
		// flaky no-op ~1.5% of the time).
		// Find the signature parameter, anchored to a query separator
		// so we cannot accidentally match an unrelated substring in the
		// path or in another query value. Order matters only for
		// disambiguation; in practice each URL has exactly one of
		// these.
		var sigParam string
		var idx int
		for _, name := range []string{"X-Amz-Signature", "X-Goog-Signature", "sig"} {
			for _, sep := range []string{"?" + name + "=", "&" + name + "="} {
				if i := strings.Index(raw, sep); i != -1 {
					sigParam, idx = sep, i
					break
				}
			}
			if sigParam != "" {
				break
			}
		}
		if sigParam == "" {
			t.Fatalf("no recognized signature parameter found in URL; cannot tamper")
		}
		pos := idx + len(sigParam)
		var tampered string
		if pos >= len(raw) {
			tampered = raw + "X"
		} else {
			replacement := byte('X')
			if raw[pos] == 'X' {
				replacement = 'Y'
			}
			tampered = raw[:pos] + string(replacement) + raw[pos+1:]
		}
		status := httpGetStatus(t, tampered)
		switch {
		case status == -1:
			// URL so corrupted the request couldn't be issued (e.g.,
			// tamper landed on a %-prefixed byte and broke URL parsing).
			// This satisfies the contract: tampered URLs do not succeed.
		case status >= 400 && status < 500:
			// Server rejected the tampered signature.
		default:
			t.Fatalf("expected 4xx or transport failure for tampered URL, got %d", status)
		}
	})

	t.Run("long_ttl_does_not_break_minting", func(t *testing.T) {
		s, cleanup := put(t)
		defer cleanup()
		raw, err := s.SignedGetURL(context.Background(), key, storage.SignedURLOptions{
			Expires: 365 * 24 * time.Hour, Method: "GET",
		})
		if err != nil {
			// Refusing very long TTLs is acceptable — either explicit
			// error or silent clamp satisfies the contract that
			// excessive TTLs do not produce a working long-lived URL.
			// We don't require a specific error class here because
			// per-SDK wrapping (S3 v2, GCS, Azure) varies; the contract
			// is "does not produce a working URL", which a non-nil err
			// trivially satisfies.
			return
		}
		if status := httpGetStatus(t, raw); status < 200 || status >= 300 {
			t.Fatalf("clamped URL did not work: status=%d", status)
		}
	})

	t.Run("expected_hash_binding", func(t *testing.T) {
		s, cleanup := put(t)
		defer cleanup()
		// This sub-test assumes any future ExpectedHash binding is
		// transparent to a bare HTTP GET: query-param embedded (like
		// S3's x-amz-checksum-mode in the presigned URL) or
		// precondition-style. A signed-header-only binding would be
		// invisible to net/http.Get and would falsely fail the
		// correct-hash assertion here. If such a strategy is ever
		// added, the per-adapter test must mint and fetch with the
		// matching client, not via RunCapabilitySigning.
		urlOK, err := s.SignedGetURL(context.Background(), key, storage.SignedURLOptions{
			Expires: 30 * time.Second, Method: "GET", ExpectedHash: correctHash,
		})
		if err != nil {
			t.Fatalf("SignedGetURL(correct hash): %v", err)
		}
		if status := httpGetStatus(t, urlOK); status < 200 || status >= 300 {
			t.Fatalf("URL with correct ExpectedHash returned %d (want 2xx)", status)
		}
		urlBad, err := s.SignedGetURL(context.Background(), key, storage.SignedURLOptions{
			Expires: 30 * time.Second, Method: "GET", ExpectedHash: wrongHash,
		})
		if err != nil {
			t.Fatalf("SignedGetURL(wrong hash): %v", err)
		}
		// Fetch the wrong-hash URL. Per spec the adapter MAY bind (4xx)
		// or MAY ignore the field (2xx); either is acceptable here.
		// Per-adapter tests assert the stronger guarantee where
		// applicable (e.g., S3 once checksum binding is provably wired
		// end-to-end). We require the URL to behave like a real signed
		// URL: a 2xx or 4xx HTTP response. A 5xx or a transport-layer
		// failure means the URL itself is malformed, which violates the
		// minting contract.
		status := httpGetStatus(t, urlBad)
		switch {
		case status >= 200 && status < 300:
			// Adapter ignores ExpectedHash — acceptable on gcs, azureblob, localfs.
		case status >= 400 && status < 500:
			// Adapter binds ExpectedHash and S3/proxy enforces — acceptable.
		default:
			t.Fatalf("wrong-hash URL produced unexpected status %d (expected 2xx or 4xx)", status)
		}
	})

	t.Run("signed_url_put_round_trip", func(t *testing.T) {
		// Verify PUT signing: mint a PUT URL, upload bytes via HTTP, then
		// Head the key and confirm size. The key includes "-put-" to avoid
		// colliding with the GET-side fixture above.
		s, cleanup := f(t)
		defer cleanup()
		putKey := "rk/m13-signing-put-" + hex.EncodeToString(bodyHash[:8])
		payload := []byte("lfs-conformance")
		raw, err := s.SignedGetURL(context.Background(), putKey, storage.SignedURLOptions{
			Expires: 30 * time.Second, Method: "PUT",
		})
		if errors.Is(err, storage.ErrNotSupported) {
			t.Skipf("adapter does not support PUT signing: %v", err)
		}
		if err != nil {
			t.Fatalf("SignedGetURL(PUT): %v", err)
		}
		status := httpPutStatus(t, raw, payload)
		if status < 200 || status >= 300 {
			t.Fatalf("PUT status = %d, want 2xx", status)
		}
		meta, err := s.Head(context.Background(), putKey)
		if err != nil {
			t.Fatalf("Head: %v", err)
		}
		if meta.Size != int64(len(payload)) {
			t.Fatalf("Head size = %d, want %d", meta.Size, len(payload))
		}
		// Clean up the object we just wrote, but tolerate adapters that
		// don't expose a stable version for this code path. The factory
		// cleanup at function exit will catch any straggler.
		if err := s.DeleteIfVersionMatches(context.Background(), putKey, meta.Version); err != nil {
			// Some adapters return ErrVersionMismatch if the metadata Version
			// field is empty for newly-uploaded objects via SignedURL. Just
			// log; the bucket cleanup handles it.
			t.Logf("DeleteIfVersionMatches (best-effort): %v", err)
		}
	})
}

func mustHTTPGet(t *testing.T, url string) []byte {
	t.Helper()
	resp, err := conformanceHTTPClient.Get(url) //nolint:gosec
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return out
}

func httpGetStatus(t *testing.T, url string) int {
	t.Helper()
	resp, err := conformanceHTTPClient.Get(url) //nolint:gosec
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// httpPutStatus performs an HTTP PUT with the given body and returns
// the status code (or -1 if the request could not be issued). Used by
// the signed_url_put_round_trip subtest.
func httpPutStatus(t *testing.T, url string, body []byte) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return -1
	}
	resp, err := conformanceHTTPClient.Do(req)
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}
