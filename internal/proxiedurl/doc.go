// Package proxiedurl mints and verifies short-lived HMAC tokens used by
// the M11 gateway-proxied URL endpoints (/_bundle/<hash>, /_pack/<hash>).
//
// Tokens are opaque base64url-encoded payloads bound to (kind, hash,
// expiry). The signing key is supplied at gateway startup
// (--proxied-url-signing-key=<file>); rotation is by replacement at
// startup time, with the operational rule that TTLs are bounded well
// under the M8 retention window (typical TTLs: 1h pack, 4h bundle).
package proxiedurl
