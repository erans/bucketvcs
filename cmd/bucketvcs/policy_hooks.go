package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/hooks"
)

const policyHooksUsage = `Usage: bucketvcs policy hooks <action> [flags]

Actions:
  add      Register or update a hook for (tenant, repo, trigger)
  list     List hooks for (tenant, repo) [optionally filtered by --trigger]
  remove   Remove a hook
  enable   Enable a previously disabled hook
  disable  Disable a hook without removing the row

Required flags for all actions: --auth-db --tenant --repo
Required flags for add/remove/enable/disable: --trigger --script
Optional for add: --order (int, default 0)
Optional for all: --actor (audit override)

Trigger values: pre-receive | post-receive

Output:
  add/remove/enable/disable  single human-readable line on stdout
  list                       NDJSON — one JSON object per line; empty list
                             emits nothing. Optional --trigger filter.

Exit codes:
  0  ok
  1  operational error (db unreachable, row not found on remove/enable/disable)
  2  usage error (missing/invalid flags, malformed trigger or script name)
`

func runPolicyHooks(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprint(stderr, policyHooksUsage)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, policyHooksUsage)
		return 0
	case "add":
		return runPolicyHooksAdd(args[1:], stdout, stderr)
	case "list":
		return runPolicyHooksList(args[1:], stdout, stderr)
	case "remove":
		return runPolicyHooksRemove(args[1:], stdout, stderr)
	case "enable":
		return runPolicyHooksSetEnabled(args[1:], stdout, stderr, true)
	case "disable":
		return runPolicyHooksSetEnabled(args[1:], stdout, stderr, false)
	default:
		fmt.Fprintf(stderr, "policy hooks: unknown action %q\n%s", args[0], policyHooksUsage)
		return 2
	}
}

func runPolicyHooksAdd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("policy hooks add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "Path to authdb (required)")
	tenant := fs.String("tenant", "", "Tenant ID (required)")
	repo := fs.String("repo", "", "Repo ID (required)")
	trigger := fs.String("trigger", "", "Trigger: pre-receive | post-receive (required)")
	script := fs.String("script", "", "Script name (required, charset [A-Za-z0-9._-])")
	order := fs.Int("order", 0, "Sort order (ascending)")
	actor := fs.String("actor", "", "Audit actor (overrides CLI user)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" || *tenant == "" || *repo == "" || *trigger == "" || *script == "" {
		fmt.Fprintln(stderr, "policy hooks add: --auth-db, --tenant, --repo, --trigger, --script required")
		return 2
	}
	if *trigger != hooks.TriggerPreReceive && *trigger != hooks.TriggerPostReceive {
		fmt.Fprintf(stderr, "policy hooks add: --trigger must be %q or %q (got %q)\n",
			hooks.TriggerPreReceive, hooks.TriggerPostReceive, *trigger)
		return 2
	}
	if !hooks.ValidScriptName(*script) {
		fmt.Fprintf(stderr, "policy hooks add: --script %q invalid (must be [A-Za-z0-9._-]+, no path separators)\n", *script)
		return 2
	}
	store, closeFn, err := openHooksStore(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "policy hooks add: %v\n", err)
		return 1
	}
	defer closeFn()
	row := hooks.Row{
		Tenant:     *tenant,
		Repo:       *repo,
		Trigger:    *trigger,
		ScriptName: *script,
		SortOrder:  *order,
		Enabled:    true,
		Now:        time.Now(),
	}
	if err := store.Add(context.Background(), row); err != nil {
		fmt.Fprintf(stderr, "policy hooks add: %v\n", err)
		return 1
	}
	hooks.EmitHookLifecycle(context.Background(), slog.Default(), "policy.hook.added",
		*tenant, *repo, *trigger, *script, *actor, *order)
	fmt.Fprintf(stdout, "added: %s/%s %s %s (order=%d)\n",
		*tenant, *repo, *trigger, *script, *order)
	return 0
}

func runPolicyHooksList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("policy hooks list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "Path to authdb (required)")
	tenant := fs.String("tenant", "", "Tenant ID (required)")
	repo := fs.String("repo", "", "Repo ID (required)")
	trigger := fs.String("trigger", "", "Optional trigger filter: pre-receive | post-receive")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" || *tenant == "" || *repo == "" {
		fmt.Fprintln(stderr, "policy hooks list: --auth-db, --tenant, --repo required")
		return 2
	}
	if *trigger != "" && *trigger != hooks.TriggerPreReceive && *trigger != hooks.TriggerPostReceive {
		fmt.Fprintf(stderr, "policy hooks list: --trigger must be %q or %q (got %q)\n",
			hooks.TriggerPreReceive, hooks.TriggerPostReceive, *trigger)
		return 2
	}
	store, closeFn, err := openHooksStore(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "policy hooks list: %v\n", err)
		return 1
	}
	defer closeFn()
	rows, err := store.List(context.Background(), *tenant, *repo, *trigger)
	if err != nil {
		fmt.Fprintf(stderr, "policy hooks list: %v\n", err)
		return 1
	}
	// NDJSON: one object per line, no enclosing array; empty list emits
	// nothing. Mirrors the refs / paths list contract (M14 / M16).
	enc := json.NewEncoder(stdout)
	for _, r := range rows {
		_ = enc.Encode(map[string]any{
			"tenant":      r.Tenant,
			"repo":        r.Repo,
			"trigger":     r.Trigger,
			"script_name": r.ScriptName,
			"sort_order":  r.SortOrder,
			"enabled":     r.Enabled,
			"created_at":  r.CreatedAt.Unix(),
			"updated_at":  r.UpdatedAt.Unix(),
		})
	}
	return 0
}

func runPolicyHooksRemove(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("policy hooks remove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "Path to authdb (required)")
	tenant := fs.String("tenant", "", "Tenant ID (required)")
	repo := fs.String("repo", "", "Repo ID (required)")
	trigger := fs.String("trigger", "", "Trigger: pre-receive | post-receive (required)")
	script := fs.String("script", "", "Script name (required, exact match)")
	actor := fs.String("actor", "", "Audit actor (overrides CLI user)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" || *tenant == "" || *repo == "" || *trigger == "" || *script == "" {
		fmt.Fprintln(stderr, "policy hooks remove: --auth-db, --tenant, --repo, --trigger, --script required")
		return 2
	}
	store, closeFn, err := openHooksStore(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "policy hooks remove: %v\n", err)
		return 1
	}
	defer closeFn()
	if err := store.Remove(context.Background(), *tenant, *repo, *trigger, *script); err != nil {
		if errors.Is(err, hooks.ErrNotFound) {
			fmt.Fprintf(stderr, "policy hooks remove: not found: %s/%s %s %s\n",
				*tenant, *repo, *trigger, *script)
			return 1
		}
		fmt.Fprintf(stderr, "policy hooks remove: %v\n", err)
		return 1
	}
	hooks.EmitHookLifecycle(context.Background(), slog.Default(), "policy.hook.removed",
		*tenant, *repo, *trigger, *script, *actor, 0)
	fmt.Fprintf(stdout, "removed: %s/%s %s %s\n", *tenant, *repo, *trigger, *script)
	return 0
}

func runPolicyHooksSetEnabled(args []string, stdout, stderr io.Writer, enabled bool) int {
	sub := "enable"
	if !enabled {
		sub = "disable"
	}
	fs := flag.NewFlagSet("policy hooks "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "Path to authdb (required)")
	tenant := fs.String("tenant", "", "Tenant ID (required)")
	repo := fs.String("repo", "", "Repo ID (required)")
	trigger := fs.String("trigger", "", "Trigger: pre-receive | post-receive (required)")
	script := fs.String("script", "", "Script name (required, exact match)")
	actor := fs.String("actor", "", "Audit actor (overrides CLI user)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *authDB == "" || *tenant == "" || *repo == "" || *trigger == "" || *script == "" {
		fmt.Fprintf(stderr, "policy hooks %s: --auth-db, --tenant, --repo, --trigger, --script required\n", sub)
		return 2
	}
	store, closeFn, err := openHooksStore(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "policy hooks %s: %v\n", sub, err)
		return 1
	}
	defer closeFn()
	if err := store.SetEnabled(context.Background(),
		*tenant, *repo, *trigger, *script, enabled, time.Now()); err != nil {
		if errors.Is(err, hooks.ErrNotFound) {
			fmt.Fprintf(stderr, "policy hooks %s: not found: %s/%s %s %s\n",
				sub, *tenant, *repo, *trigger, *script)
			return 1
		}
		fmt.Fprintf(stderr, "policy hooks %s: %v\n", sub, err)
		return 1
	}
	verb := "disabled"
	event := "policy.hook.disabled"
	if enabled {
		verb = "enabled"
		event = "policy.hook.enabled"
	}
	hooks.EmitHookLifecycle(context.Background(), slog.Default(), event,
		*tenant, *repo, *trigger, *script, *actor, 0)
	fmt.Fprintf(stdout, "%s: %s/%s %s %s\n", verb, *tenant, *repo, *trigger, *script)
	return 0
}

// openHooksStore opens the M4 authdb and returns a hooks.Store backed by its
// *sql.DB plus a close function that releases the underlying sqlitestore.
// Mirrors the openPolicySvc helper used by M14 / M16 CLI handlers but returns
// a hooks-specific store (no policy.Service in the Tier 3 path).
func openHooksStore(authDB string) (*hooks.Store, func(), error) {
	authS, err := sqlitestore.Open(authDB)
	if err != nil {
		return nil, nil, fmt.Errorf("open authdb: %w", err)
	}
	return hooks.NewStore(authS.DB()), func() { _ = authS.Close() }, nil
}
