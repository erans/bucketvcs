// Package webhooks implements spec §24 Tier 1 broad: durable, signed,
// retryable webhook delivery from bucketvcs to operator-registered HTTP
// receivers.
//
// The package owns a typed event taxonomy (see event.go) and a sqlite-backed
// durable queue (migration 0006 on the M4 authdb). A single background worker
// (worker.go) drains the queue with exponential backoff and HMAC-SHA256
// signing. The Service is optional everywhere — a nil *Service at every
// integration point produces a no-op so pre-M15 deployments are unchanged.
package webhooks
