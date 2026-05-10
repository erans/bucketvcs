// Package gc implements the M8 garbage collector for bucketvcs repos.
// See docs/superpowers/specs/2026-05-09-m8-basic-gc-design.md.
//
// The public surface is Run, which executes mark and sweep phases for a
// single repo. CLI orchestration over multiple repos lives in
// cmd/bucketvcs/gc.go and uses DiscoverRepos plus per-repo Run.
package gc
