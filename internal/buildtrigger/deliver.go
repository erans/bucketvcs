package buildtrigger

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

// Deliverer performs one attempt to start a build. Implementations are chosen
// by trigger kind. A non-nil error means the attempt failed and should be
// retried per the backoff schedule; statusCode is advisory (HTTP code or 0).
type Deliverer interface {
	Deliver(ctx context.Context, tr Trigger, p BuildPayload) (statusCode int, err error)
}

// MintFunc mints a fresh short-lived bvts for a trigger at delivery time.
type MintFunc func(ctx context.Context, tr Trigger, p BuildPayload) (string, error)

// httpDeliverer handles KindGeneric and KindCloudBuild (signed JSON POST).
type httpDeliverer struct {
	client *http.Client
	mintFn MintFunc
}

func (d *httpDeliverer) Deliver(ctx context.Context, tr Trigger, p BuildPayload) (int, error) {
	url := webhookURL(tr)
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return 0, permanentf("egress denied: trigger URL scheme must be http or https")
	}
	var token string
	if tr.TokenMode == TokenInject {
		tok, err := d.mintFn(ctx, tr, p)
		if err != nil {
			return 0, fmt.Errorf("mint token: %w", err)
		}
		token = tok
	}
	body, err := RenderBody(tr.Kind, p, token)
	if err != nil {
		return 0, fmt.Errorf("render body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "bucketvcs-buildtrigger/1")
	// Sign only when a secret is configured. generic/cloudbuild always have a
	// secret (auto-generated at Create), so their behavior is unchanged;
	// azurewebhook may be unsigned.
	if tr.Config.Secret != "" {
		prof := sigProfileFor(tr)
		req.Header.Set(prof.header, prof.sign(tr.Config.Secret, body, time.Now().Unix()))
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.CopyN(io.Discard, resp.Body, 512)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp.StatusCode, nil
	}
	if httpStatusPermanent(resp.StatusCode) {
		return resp.StatusCode, permanentf("HTTP %d", resp.StatusCode)
	}
	return resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
}

// sigProfile selects the signature header name and value format for an
// httpDeliverer kind. generic/cloudbuild keep the M15 SHA-256 scheme; Azure
// webhooks use GitHub-compatible HMAC-SHA1 over the raw body.
type sigProfile struct {
	header string
	sign   func(secret string, body []byte, t int64) string
}

func sigProfileFor(tr Trigger) sigProfile {
	if tr.Kind == KindAzureWebhook {
		header := tr.Config.AzureSigHeader
		if header == "" {
			header = "X-Hub-Signature"
		}
		return sigProfile{header: header, sign: signAzureSHA1}
	}
	return sigProfile{
		header: "BucketVCS-Signature",
		sign:   func(secret string, body []byte, t int64) string { return webhooks.Sign(secret, t, body) },
	}
}

// signAzureSHA1 returns "sha1=<hex(HMAC-SHA1(secret, body))>" — the format
// Azure DevOps incoming webhooks verify against the configured header. Azure
// signs the raw body only (no timestamp prefix), so t is ignored.
func signAzureSHA1(secret string, body []byte, _ int64) string {
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write(body)
	return "sha1=" + hex.EncodeToString(mac.Sum(nil))
}

// webhookURL returns the POST target for an httpDeliverer kind. azurewebhook
// uses Config.AzureWebhookURL; generic/cloudbuild use Config.URL.
func webhookURL(tr Trigger) string {
	if tr.Kind == KindAzureWebhook {
		return tr.Config.AzureWebhookURL
	}
	return tr.Config.URL
}

// ErrPermanent marks a delivery error that must NOT be retried (a configuration
// error or a non-transient 4xx). recordResult routes it straight to dead_letter
// regardless of attempt count.
var ErrPermanent = errors.New("permanent delivery error")

// permanentf wraps a formatted error so errors.Is(err, ErrPermanent) is true.
func permanentf(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrPermanent}, a...)...)
}

// httpStatusPermanent reports whether an HTTP status is a permanent failure:
// any 4xx except 408 (Request Timeout) and 429 (Too Many Requests), which are
// transient and should be retried.
func httpStatusPermanent(code int) bool {
	return code >= 400 && code < 500 && code != 408 && code != 429
}
