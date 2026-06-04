package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/webhooks"
)

func runRepo(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: bucketvcs repo <register|grant|revoke|public|list|delete|rename|deploy-key>")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "register":
		return repoRegister(ctx, rest, stdout, stderr)
	case "grant":
		return repoGrant(ctx, rest, stdout, stderr)
	case "revoke":
		return repoRevoke(ctx, rest, stdout, stderr)
	case "public":
		return repoPublic(ctx, rest, stdout, stderr)
	case "list":
		return repoList(ctx, rest, stdout, stderr)
	case "delete":
		return repoDelete(ctx, rest, stdout, stderr)
	case "rename":
		return repoRename(ctx, rest, stdout, stderr)
	case "deploy-key":
		return runRepoDeployKey(ctx, rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "repo: unknown subcommand %q\n", sub)
		return 2
	}
}

// splitTenantRepo parses a single "<tenant>/<repo>" CLI argument. Both
// segments are validated against the same charset the gateway uses for
// route names (^[A-Za-z0-9._-]+$). Strings with extra slashes ("a/b/c"),
// empty segments ("a/"), or characters outside that charset are rejected
// — the gateway route parser only accepts /{tenant}/{repo}.git, so any
// CLI input that survives splitTenantRepo but fails the gateway's filter
// would produce a useless registration.
func splitTenantRepo(s string) (string, string, error) {
	parts := strings.Split(s, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("expected tenant/repo, got %q", s)
	}
	if !validName(parts[0]) || !validName(parts[1]) {
		return "", "", fmt.Errorf("tenant/repo segments must match [A-Za-z0-9._-]+, got %q", s)
	}
	return parts[0], parts[1], nil
}

// validName reports whether s matches ^[A-Za-z0-9._-]+$ — the same charset
// the gateway's nameRE accepts for tenant/repo path segments. Empty input
// is rejected by the caller (splitTenantRepo) before we get here, but we
// also reject "" defensively in case future callers add new entry points.
func validName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z',
			r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			// ok
		default:
			return false
		}
	}
	return true
}

func repoRegister(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repo register", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")
	noInit := fs.Bool("no-init", false, "skip M1 bucket init (registry only)")
	storeURL := fs.String("store", "", `Store URL for M1 init (e.g. "localfs:/var/lib/bucketvcs"); ignored with --no-init`)

	// --actor=<string>: explicit operator override for the webhook + audit
	// actor field. Distinguish "not passed" (use cliActor) from "--actor="
	// (empty, usage error) via a fs.Func closure.
	actorPassed := false
	actor := ""
	fs.Func("actor", "override actor for webhook + audit events (default: OS username)",
		func(s string) error {
			actorPassed = true
			actor = s
			return nil
		})

	fs.SetOutput(stderr)
	if err := fs.Parse(reorderFlagsFirst(args, map[string]bool{"no-init": true})); err != nil {
		return 2
	}
	if actorPassed && actor == "" {
		fmt.Fprintln(stderr, "register: --actor must be non-empty if specified")
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: bucketvcs repo register <tenant>/<repo> [--no-init] [--store <url>]")
		return 2
	}
	tenant, repo, err := splitTenantRepo(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if !*noInit {
		if *storeURL == "" {
			fmt.Fprintln(stderr, "register: --store is required (or pass --no-init for registry-only)")
			return 2
		}
		initArgs := []string{"--store", *storeURL, tenant, repo}
		if rc := runInit(ctx, initArgs, stdout, stderr); rc != 0 {
			fmt.Fprintln(stderr, "init failed; if the bucket repo already exists, retry with --no-init")
			return 1
		}
	}
	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()
	// RegisterRepoIfNew is INSERT OR IGNORE under the hood and reports
	// whether a new row was actually inserted — TOCTOU-free vs a separate
	// GetRepoFlags pre-check.
	inserted, err := s.RegisterRepoIfNew(ctx, tenant, repo)
	if err != nil {
		fmt.Fprintf(stderr, "register: %v\n", err)
		return 1
	}
	if inserted {
		webhookSvc := webhooks.New(s.DB())
		if werr := webhookSvc.Enqueue(ctx, webhooks.EventRepoCreated,
			tenant, repo, actorOrDefault(actor), webhooks.RepoLifecyclePayload{}); werr != nil {
			webhooks.EmitEnqueueFailed(ctx, nil, tenant, repo, "repo.created", werr.Error())
			fmt.Fprintf(stderr, "warning: webhooks.enqueue_failed for repo.created: %v\n", werr)
		}
	}
	return 0
}

func repoGrant(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repo grant", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")

	actorPassed := false
	actor := ""
	fs.Func("actor", "reserved for future audit/webhook emissions; currently has no effect on this subcommand",
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
		fmt.Fprintln(stderr, "grant: --actor must be non-empty if specified")
		return 2
	}
	if fs.NArg() != 3 {
		fmt.Fprintln(stderr, "usage: bucketvcs repo grant <user> <tenant>/<repo> <read|write|admin>")
		return 2
	}
	user := fs.Arg(0)
	tenant, repo, err := splitTenantRepo(fs.Arg(1))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	perm := fs.Arg(2)
	_ = actor // currently unused; parsed for forward-compat with future audit/webhook emissions on grant/revoke/public
	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()
	if err := s.Grant(ctx, user, tenant, repo, perm); err != nil {
		if errors.Is(err, auth.ErrNoSuchRepo) {
			fmt.Fprintln(stderr, "repo not registered (run `bucketvcs repo register`)")
			return 1
		}
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	return 0
}

func repoRevoke(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repo revoke", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")

	actorPassed := false
	actor := ""
	fs.Func("actor", "reserved for future audit/webhook emissions; currently has no effect on this subcommand",
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
		fmt.Fprintln(stderr, "revoke: --actor must be non-empty if specified")
		return 2
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(stderr, "usage: bucketvcs repo revoke <user> <tenant>/<repo>")
		return 2
	}
	user := fs.Arg(0)
	tenant, repo, err := splitTenantRepo(fs.Arg(1))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	_ = actor // currently unused; parsed for forward-compat with future audit/webhook emissions on grant/revoke/public
	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()
	if err := s.RevokeRepoPermission(ctx, user, tenant, repo); err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	return 0
}

func repoPublic(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repo public", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")

	actorPassed := false
	actor := ""
	fs.Func("actor", "reserved for future audit/webhook emissions; currently has no effect on this subcommand",
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
		fmt.Fprintln(stderr, "public: --actor must be non-empty if specified")
		return 2
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(stderr, "usage: bucketvcs repo public <tenant>/<repo> <on|off>")
		return 2
	}
	tenant, repo, err := splitTenantRepo(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	on := false
	switch fs.Arg(1) {
	case "on":
		on = true
	case "off":
		on = false
	default:
		fmt.Fprintln(stderr, "expected on|off")
		return 2
	}
	_ = actor // currently unused; parsed for forward-compat with future audit/webhook emissions on grant/revoke/public
	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()
	if err := s.SetRepoPublic(ctx, tenant, repo, on); err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	return 0
}

func repoList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repo list", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")
	tenant := fs.String("tenant", "", "filter by tenant")
	fs.SetOutput(stderr)
	if err := fs.Parse(reorderFlagsFirst(args, nil)); err != nil {
		return 2
	}
	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()
	rows, err := s.ListRepos(ctx, *tenant)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "tenant\tname\tpublic\tcreated")
	for _, r := range rows {
		pub := "no"
		if r.PublicRead {
			pub = "yes"
		}
		fmt.Fprintf(stdout, "%s\t%s\t%s\t%d\n", r.Tenant, r.Name, pub, r.CreatedAt)
	}
	return 0
}

// cliActor returns a best-effort display name for the OS user running the
// CLI. Used to attribute webhook events emitted from CLI subcommands
// (M15 Task 5). Falls back to "cli" if the OS user lookup fails or the
// environment lacks both USER and LOGNAME.
//
// PRIVACY (M15.1 Task 2): this value is emitted as the `actor` field of
// repo-lifecycle webhooks sent to operator-configured external receivers.
// On shared hosts the OS username may be `root`, a service account name,
// or otherwise reveal infra topology. Operators who want to hide the host
// identity can pass `--actor=<string>` to the affected repo subcommands
// (register/grant/revoke/public) or run as a dedicated service account.
func cliActor() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	for _, env := range []string{"USER", "LOGNAME"} {
		if v := os.Getenv(env); v != "" {
			return v
		}
	}
	return "cli"
}

// actorOrDefault returns the operator-supplied --actor value if non-empty,
// or cliActor() (OS username) when the flag wasn't passed. Empty values
// from an explicit --actor= are rejected by the caller before reaching
// this helper.
func actorOrDefault(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	return cliActor()
}

// repoDelete implements `bucketvcs repo delete`.
//
// Order of operations (deviates from design spec §2.2 — spec listed
// auth-then-storage; this implementation flips to storage-then-auth so an
// operator who hits a partial `--purge-storage` failure can retry the
// same command while the auth row still exists):
//
//  1. Verify the repo exists (return ErrNoSuchRepo before any side effects).
//  2. Optional --purge-storage: iterate the object store and delete every
//     key under <tenant>/<repo>/. Per-key errors are non-fatal.
//  3. Enqueue the repo.deleted webhook delivery. This MUST happen before
//     step 4 because webhook_endpoints rows for (tenant, repo) are scoped
//     to the repo we are about to delete — if we delete first, the cascade
//     drops the endpoints and the subsequent Enqueue finds no rows to
//     insert deliveries against.
//  4. Manually delete dependent rows (protected_refs, repo_permissions,
//     ssh deploy keys) and then the repos row with FK pragma OFF. We
//     deliberately do NOT cascade-delete webhook_endpoints +
//     webhook_deliveries — those must survive the repos row long enough
//     for the webhook worker to deliver. The orphan webhook_endpoints row
//     (now referencing a non-existent (tenant, repo)) is a known
//     limitation; a future webhook-prune job will sweep them. With
//     foreign_keys=OFF the orphan does not produce an integrity error;
//     the worker JOIN against the orphan endpoint still resolves and the
//     delivery completes.
//
// Re-registration pitfall: webhook_endpoints rows for the deleted
// (tenant, repo) are intentionally retained as orphans so the worker can
// drain the just-enqueued repo.deleted delivery. WARNING: if the same
// (tenant, repo) is later re-registered via `bucketvcs repo register`,
// those orphan endpoints become active subscriptions for the new repo,
// potentially leaking events to receivers that were configured for the
// previous incarnation (and signed with the previous incarnation's
// endpoint secret). Operators re-registering should run
// `bucketvcs webhook endpoint list --tenant=<t> --repo=<r>` first and
// `webhook endpoint remove` any unwanted carry-overs. A future milestone
// will add an automated webhook-prune sweep for endpoints whose
// (tenant, repo) has no matching `repos` row AND zero pending deliveries.
func repoDelete(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repo delete", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")
	purgeStorage := fs.Bool("purge-storage", false, "also iterate the storage backend and delete every object under <tenant>/<repo>/")
	storeURL := fs.String("store", "", `Store URL for --purge-storage (e.g. "localfs:/var/lib/bucketvcs"); ignored without --purge-storage`)

	actorPassed := false
	actor := ""
	fs.Func("actor", "override actor for webhook + audit events (default: OS username)",
		func(s string) error {
			actorPassed = true
			actor = s
			return nil
		})

	fs.SetOutput(stderr)
	if err := fs.Parse(reorderFlagsFirst(args, map[string]bool{"purge-storage": true})); err != nil {
		return 2
	}
	if actorPassed && actor == "" {
		fmt.Fprintln(stderr, "delete: --actor must be non-empty if specified")
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: bucketvcs repo delete <tenant>/<repo> [--purge-storage --store <url>] [--actor <name>]")
		return 2
	}
	tenant, repo, err := splitTenantRepo(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if *purgeStorage && *storeURL == "" {
		fmt.Fprintln(stderr, "delete --purge-storage: --store is required")
		return 2
	}
	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()

	// Step 1: existence check. DeleteRepo silently no-ops when the row is
	// absent, so we explicitly probe via GetRepoFlags to honor the
	// "no such repo → exit 1" contract in spec §2.3.
	if _, err := s.GetRepoFlags(ctx, tenant, repo); err != nil {
		if errors.Is(err, auth.ErrNoSuchRepo) {
			fmt.Fprintln(stderr, "no such repo")
			return 1
		}
		fmt.Fprintf(stderr, "delete: %v\n", err)
		return 1
	}

	// Step 2: optional storage purge (BEFORE auth — see godoc above).
	var purgedKeys, purgeErrors int64
	if *purgeStorage {
		bs, err := openStore(*storeURL)
		if err != nil {
			fmt.Fprintf(stderr, "delete --purge-storage: %v\n", err)
			return 1
		}
		defer closeStore(bs)
		purgedKeys, purgeErrors, err = purgePrefix(ctx, bs, tenant+"/"+repo+"/")
		if err != nil {
			fmt.Fprintf(stderr, "delete --purge-storage: %v\n", err)
			return 1
		}
	}
	// purgeErrors > 0 means partial-purge failure mid-pagination. We still
	// proceed with the auth-row delete + webhook enqueue (the repo is
	// unreachable either way), but exit 1 at the end so automation detects
	// the partial state.
	partialPurgeFailure := purgeErrors > 0

	// Step 3: enqueue the repo.deleted webhook BEFORE step 4 so the
	// SELECT in Enqueue still finds the endpoint rows for (tenant, repo).
	webhookSvc := webhooks.New(s.DB())
	actorVal := actorOrDefault(actor)
	if werr := webhookSvc.Enqueue(ctx, webhooks.EventRepoDeleted,
		tenant, repo, actorVal, webhooks.RepoLifecyclePayload{}); werr != nil {
		webhooks.EmitEnqueueFailed(ctx, nil, tenant, repo, "repo.deleted", werr.Error())
		fmt.Fprintf(stderr, "warning: webhooks.enqueue_failed for repo.deleted: %v\n", werr)
	}

	// Step 4: manually orchestrate the delete so webhook tables survive.
	// Use a tx with foreign_keys=OFF; clean up the non-webhook dependents
	// explicitly, then drop the repos row. PRAGMA foreign_keys is per-
	// connection in sqlite, so we set/restore around the transaction.
	if err := s.DeleteRepoCascade(ctx, tenant, repo); err != nil {
		fmt.Fprintf(stderr, "delete: %v\n", err)
		return 1
	}

	if *purgeStorage {
		fmt.Fprintf(stdout, "tenant=%s  repo=%s  deleted  purged=%d keys  errors=%d\n",
			tenant, repo, purgedKeys, purgeErrors)
	} else {
		fmt.Fprintf(stdout, "tenant=%s  repo=%s  deleted\n", tenant, repo)
	}
	if partialPurgeFailure {
		return 1
	}
	return 0
}

// purgePrefix iterates the storage prefix and deletes every key. Returns
// (purgedCount, errorCount, fatalErr). fatalErr is non-nil only when the
// initial List call fails with no progress; per-key errors and subsequent
// List failures increment errs and continue.
func purgePrefix(ctx context.Context, store storage.ObjectStore, prefix string) (int64, int64, error) {
	var purged, errs int64
	var nextToken string
	for {
		opts := &storage.ListOptions{
			MaxKeys:           1000,
			ContinuationToken: nextToken,
		}
		page, err := store.List(ctx, prefix, opts)
		if err != nil {
			if purged == 0 && errs == 0 {
				return 0, 0, fmt.Errorf("list: %w", err)
			}
			return purged, errs + 1, nil
		}
		for _, obj := range page.Objects {
			if derr := store.DeleteIfVersionMatches(ctx, obj.Key, obj.Version); derr != nil {
				errs++
				continue
			}
			purged++
		}
		if page.NextToken == "" {
			break
		}
		nextToken = page.NextToken
	}
	return purged, errs, nil
}
