// Package buildtrigger fires durable, ref-filtered HTTP requests on push to
// start CI builds (GCP Cloud Build, AWS CodeBuild), optionally minting a
// short-lived, single-repo pull token. It mirrors the M15 webhooks delivery
// engine (claim/backoff/dead-letter/replay) and reuses M16's `**`-aware ref
// matcher and M22's repo-scoped token minting. M15 webhooks are untouched.
package buildtrigger
