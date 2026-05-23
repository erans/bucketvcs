package webhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// Sign returns the value of the BucketVCS-Signature header for body signed
// with secret at unix time t. Format:
//
//	t=<unix>,v1=<hex(HMAC-SHA256(secret, "<t>.<body>"))>
//
// Receivers verify by re-running HMAC over <current t>.<body>. The 5-min
// replay window (header value of t vs receiver wall clock) is enforced on
// the receiver side; the worker re-signs with the current t per attempt so
// the signature stays within that window even across 14h of backoff.
func Sign(secret string, t int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(t, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return "t=" + strconv.FormatInt(t, 10) + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}
