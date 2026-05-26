package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

// repoRename implements `bucketvcs repo rename`. It is a same-tenant
// auth-only rename: the auth.db row + every FK-bearing dependent table
// (repo_permissions, deploy_keys, protected_refs, webhook_endpoints,
// protected_paths, etc.) are atomically renamed via sqlitestore.RenameRepo.
//
// Storage keys are NOT migrated. After rename the storage backend still
// holds blobs at tenants/<tenant>/repos/<old-name>/... — operators handle
// storage migration out of band. To prevent a future read from the new
// name accidentally picking up unrelated leftover keys, we refuse to
// rename if any keys already exist under the destination prefix.
//
// Cross-tenant rename is intentionally not supported in M21: the
// <new-name> argument is a bare segment with no slash. Adding a "/" would
// imply cross-tenant motion and is rejected at the CLI surface.
//
// Webhook ordering: the repo.renamed delivery is enqueued BEFORE
// RenameRepo runs (matching the M15.1 repo.deleted precedent). The
// webhook_endpoints row scoped to (tenant, old-name) is still in place
// when Enqueue resolves subscribers — RenameRepo would carry the
// endpoints over to the new name in the same transaction, but a
// subscriber that filtered on the old (tenant, repo) at message-receive
// time would not match against post-rename rows. Enqueue-first locks in
// the SELECT against the old name.
//
// If the auth transaction fails AFTER the webhook is enqueued, the
// delivery still goes out (worker doesn't know the rename was rolled
// back). This is documented in the spec §5 (M21 webhook prune + repo
// rename design); operators should treat repo.renamed as an at-least-
// once signal and reconcile via subsequent state queries.
//
// Outcome metrics: 5 outcomes are reported via
// webhooks.EmitRepoRenamedMetric — ok, collision_auth, collision_storage,
// not_found, cross_tenant. Each invocation emits exactly one sample.
func repoRename(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repo rename", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")
	storeURL := fs.String("store", "", `storage URL for the destination-prefix collision check (e.g. "localfs:/var/lib/bucketvcs")`)

	actorPassed := false
	actor := ""
	fs.Func("actor", "override actor for webhook + audit events (default: OS username)",
		func(s string) error {
			actorPassed = true
			actor = s
			return nil
		})

	fs.SetOutput(stderr)
	if err := fs.Parse(reorderFlagsFirst(args, nil)); err != nil {
		return 2
	}
	if actorPassed && actor == "" {
		fmt.Fprintln(stderr, "rename: --actor must be non-empty if specified")
		return 2
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(stderr, "usage: bucketvcs repo rename <tenant>/<old-name> <new-name> --auth-db=<path> --store=<url> [--actor <name>]")
		fmt.Fprintln(stderr, "note: --store opens the backend for a destination-prefix collision probe;")
		fmt.Fprintln(stderr, "      on localfs this acquires an exclusive whole-bucket lock, so the gateway")
		fmt.Fprintln(stderr, "      must be stopped during the rename. Cloud backends have no such lock.")
		return 2
	}
	tenant, oldName, err := splitTenantRepo(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	newName := fs.Arg(1)
	// CLI surface guard: cross-tenant impossible by parsing — <new-name>
	// must be a bare segment. Reject any path separator before validName so
	// the diagnostic mentions cross-tenant explicitly.
	if strings.ContainsAny(newName, `/\`) {
		fmt.Fprintln(stderr, "rename: <new-name> must be a bare segment; cross-tenant rename is not supported")
		webhooks.EmitRepoRenamedMetric(ctx, slog.Default(), "cross_tenant")
		return 2
	}
	if !validName(newName) {
		fmt.Fprintf(stderr, "rename: <new-name> must match [A-Za-z0-9._-]+, got %q\n", newName)
		return 2
	}
	if newName == oldName {
		fmt.Fprintln(stderr, "rename: <new-name> equals <old-name>; no-op")
		return 2
	}
	if *authDB == "" {
		fmt.Fprintln(stderr, "rename: --auth-db is required")
		return 2
	}
	if *storeURL == "" {
		fmt.Fprintln(stderr, "rename: --store is required (for destination-prefix collision check)")
		return 2
	}

	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()

	// Pre-check 1: source must exist. Without this, RenameRepo would
	// return ErrNoSuchRepo and we would report it, but we also want the
	// "not_found" metric outcome to fire even when no rename was attempted.
	if _, err := s.GetRepoFlags(ctx, tenant, oldName); err != nil {
		if errors.Is(err, auth.ErrNoSuchRepo) {
			fmt.Fprintf(stderr, "rename: %s/%s not found\n", tenant, oldName)
			webhooks.EmitRepoRenamedMetric(ctx, slog.Default(), "not_found")
			return 1
		}
		fmt.Fprintf(stderr, "rename: %v\n", err)
		return 1
	}

	// Pre-check 2: destination auth row must not already exist.
	// RenameRepo also catches this via ErrRepoExists, but the early check
	// gives a cleaner error message before we open the storage backend.
	if _, err := s.GetRepoFlags(ctx, tenant, newName); err == nil {
		fmt.Fprintf(stderr, "rename: destination %s/%s already exists in auth.db\n", tenant, newName)
		webhooks.EmitRepoRenamedMetric(ctx, slog.Default(), "collision_auth")
		return 1
	} else if !errors.Is(err, auth.ErrNoSuchRepo) {
		fmt.Fprintf(stderr, "rename: destination probe: %v\n", err)
		return 1
	}

	// Pre-check 3: destination storage prefix must be empty. We do not
	// migrate keys — leaving unrelated objects in place under the new
	// name's prefix would cause confused reads after rename.
	bs, err := openStore(*storeURL)
	if err != nil {
		fmt.Fprintf(stderr, "rename: store: %v\n", err)
		return 1
	}
	defer closeStore(bs)
	destPrefix := "tenants/" + tenant + "/repos/" + newName + "/"
	page, err := bs.List(ctx, destPrefix, &storage.ListOptions{MaxKeys: 1})
	if err != nil {
		fmt.Fprintf(stderr, "rename: storage collision check: %v\n", err)
		return 1
	}
	if page != nil && len(page.Objects) > 0 {
		fmt.Fprintf(stderr, "rename: storage prefix %s is non-empty (first key: %s); refusing to rename\n",
			destPrefix, page.Objects[0].Key)
		webhooks.EmitRepoRenamedMetric(ctx, slog.Default(), "collision_storage")
		return 1
	}

	// Enqueue the repo.renamed webhook BEFORE RenameRepo runs. Endpoints
	// scoped to (tenant, oldName) are still in webhook_endpoints — Enqueue
	// joins on (tenant, repo) and would miss them after the rename moves
	// the rows to the new name. Matches M15.1 repo.deleted ordering.
	//
	// Fail-open: a webhook enqueue failure does not block the rename. We
	// emit the standard webhooks.enqueue_failed audit signal and proceed.
	resolvedActor := actorOrDefault(actor)
	webhookSvc := webhooks.New(s.DB())
	payload := webhooks.RepoRenamedPayload{
		OldName: oldName,
		NewName: newName,
	}
	if werr := webhookSvc.Enqueue(ctx, webhooks.EventRepoRenamed,
		tenant, oldName, resolvedActor, payload); werr != nil {
		webhooks.EmitEnqueueFailed(ctx, nil, tenant, oldName, "repo.renamed", werr.Error())
		fmt.Fprintf(stderr, "warning: webhooks.enqueue_failed for repo.renamed: %v\n", werr)
	}

	// Perform the atomic auth-side rename. RenameRepo handles every
	// FK-bearing dependent table inside a single transaction (see M21
	// Task 2).
	if err := s.RenameRepo(ctx, tenant, oldName, newName); err != nil {
		switch {
		case errors.Is(err, sqlitestore.ErrRepoExists):
			fmt.Fprintf(stderr, "rename: destination %s/%s appeared during rename\n", tenant, newName)
			webhooks.EmitRepoRenamedMetric(ctx, slog.Default(), "collision_auth")
			return 1
		case errors.Is(err, auth.ErrNoSuchRepo):
			fmt.Fprintf(stderr, "rename: source %s/%s disappeared during rename\n", tenant, oldName)
			webhooks.EmitRepoRenamedMetric(ctx, slog.Default(), "not_found")
			return 1
		default:
			fmt.Fprintf(stderr, "rename: %v\n", err)
			return 1
		}
	}

	webhooks.EmitRepoRenamedMetric(ctx, slog.Default(), "ok")
	slog.Default().LogAttrs(ctx, slog.LevelInfo, "repo.renamed",
		slog.String("tenant", tenant),
		slog.String("old_name", oldName),
		slog.String("new_name", newName),
		slog.String("actor", resolvedActor),
	)
	fmt.Fprintf(stdout, "renamed: %s/%s -> %s/%s\n", tenant, oldName, tenant, newName)
	return 0
}
