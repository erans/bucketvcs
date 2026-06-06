package shiplog

import (
	"encoding/json"
	"time"
)

// Usage kinds. Note: clone is deliberately folded into fetch — they are
// indistinguishable at the transport layer (spec deviation, documented).
const (
	KindFetch       = "fetch"
	KindPush        = "push"
	KindLFSUpload   = "lfs_upload"
	KindLFSDownload = "lfs_download"
	KindBundleServe = "bundle_serve"
	KindPackServe   = "pack_serve"
)

// UsageEvent is one metering record (usage stream, schema v1).
type UsageEvent struct {
	Kind       string `json:"kind"`
	Tenant     string `json:"tenant"`
	Repo       string `json:"repo"`
	Actor      string `json:"actor"`
	Transport  string `json:"transport"` // https|ssh
	Bytes      int64  `json:"bytes"`
	DurationMS int64  `json:"duration_ms"`
	Status     string `json:"status"`            // ok|error|negotiated
	Objects    int    `json:"objects,omitempty"` // LFS batch object count
}

type usageWire struct {
	V  int    `json:"v"`
	TS string `json:"ts"`
	UsageEvent
}

// Usage enqueues one metering record. Nil-engine and marshal failures are
// silent no-ops: metering must never affect serving.
func (e *Engine) Usage(ev UsageEvent) {
	if e == nil {
		return
	}
	line, err := json.Marshal(usageWire{V: 1, TS: e.cfg.Now().UTC().Format(time.RFC3339Nano), UsageEvent: ev})
	if err != nil {
		return
	}
	e.Enqueue(StreamUsage, line)
}
