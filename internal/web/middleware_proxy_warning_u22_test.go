package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProxyHeaderMismatchWarning_WarnsOnce_U22(t *testing.T) {
	logger, sink := newTestLogger()
	h := NewHandler(Deps{Store: newFakeStore(), Logger: logger, TrustProxy: false})

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/login", nil)
		req.Header.Set("X-Forwarded-Proto", "https")
		h.ServeHTTP(httptest.NewRecorder(), req)
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	const want = "web: X-Forwarded-Proto seen but --trust-proxy-headers not set; Secure cookies may be downgraded"
	count := 0
	for _, rec := range sink.records {
		if rec.Message == want {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("warning count = %d, want 1", count)
	}
}

func TestProxyHeaderMismatchWarning_TrustedProxySilent_U22(t *testing.T) {
	logger, sink := newTestLogger()
	h := NewHandler(Deps{Store: newFakeStore(), Logger: logger, TrustProxy: true})
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	h.ServeHTTP(httptest.NewRecorder(), req)

	sink.mu.Lock()
	defer sink.mu.Unlock()
	const warning = "web: X-Forwarded-Proto seen but --trust-proxy-headers not set; Secure cookies may be downgraded"
	for _, rec := range sink.records {
		if rec.Message == warning {
			t.Fatal("trusted proxy emitted mismatch warning")
		}
	}
}
