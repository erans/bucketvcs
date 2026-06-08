package buildtrigger

import (
	"bytes"
	"context"
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
	if !strings.HasPrefix(tr.Config.URL, "http://") && !strings.HasPrefix(tr.Config.URL, "https://") {
		return 0, fmt.Errorf("egress denied: trigger URL scheme must be http or https")
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
	now := time.Now().Unix()
	sig := webhooks.Sign(tr.Config.Secret, now, body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tr.Config.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("BucketVCS-Signature", sig)
	req.Header.Set("User-Agent", "bucketvcs-buildtrigger/1")
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.CopyN(io.Discard, resp.Body, 512)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
}
