// Package s3compat implements storage.ObjectStore against any
// S3-compatible object store via aws-sdk-go-v2. M5 ships this adapter
// as the canonical Cloudflare R2 backend and exercises it against AWS
// S3 to validate generalization; AWS S3 is formally promoted to a
// canonical backend at M7.
//
// The CLI exposes two schemes that route to this package:
//
//   s3://<bucket>[/<prefix>]   AWS S3 defaults (vhost addressing, no
//                              endpoint override required)
//   r2://<bucket>[/<prefix>]   Cloudflare R2 defaults (region "auto",
//                              path-style addressing, endpoint env required)
//
// All credentials come from the AWS SDK default credential chain
// (env vars, shared profile). Credentials are never URL-embedded.
//
// See docs/superpowers/specs/2026-05-07-m5-first-cloud-backend-design.md
// for the design rationale.
package s3compat
