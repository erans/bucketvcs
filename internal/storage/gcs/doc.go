// Package gcs implements storage.ObjectStore against Google Cloud
// Storage via cloud.google.com/go/storage. M7 ships this adapter as
// a canonical bucketvcs storage backend (§11.1).
//
// The CLI exposes one scheme that routes to this package:
//
//	gcs://<bucket>[/<prefix>]
//
// Credentials come from Application Default Credentials by default
// (env vars, workload identity, GCE/GKE metadata). Static credentials
// can be supplied via Config.CredentialsJSON or Config.CredentialsFile.
// Credentials are never URL-embedded.
//
// See docs/superpowers/specs/2026-05-09-m7-remaining-cloud-backends-design.md
// for the design rationale.
package gcs
