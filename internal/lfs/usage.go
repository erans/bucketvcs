package lfs

import "github.com/bucketvcs/bucketvcs/internal/shiplog"

// UsageSink receives operation-metering events from the LFS handlers
// (lfs_download from Batch, lfs_upload from verify). Defined here as a small
// interface so handler tests can fake it without importing the shipping
// engine; *shiplog.Engine satisfies it and its Usage is nil-safe. Call sites
// MUST nil-check before invoking, because the field is nil whenever log
// shipping is disabled.
type UsageSink interface {
	Usage(shiplog.UsageEvent)
}
