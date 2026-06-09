package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"time"
)

// runRepoAlias dispatches `bucketvcs repo alias <list|remove>`.
func runRepoAlias(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: bucketvcs repo alias <list|remove>")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return repoAliasList(ctx, rest, stdout, stderr)
	case "remove":
		return repoAliasRemove(ctx, rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "repo alias: unknown subcommand %q (want list|remove)\n", sub)
		return 2
	}
}

// repoAliasList implements `bucketvcs repo alias list --auth-db=<path>
// <tenant>/<name> [--format=text|json]`.
//
// Lists all aliases whose target is (tenant, name), ordered by old_name.
// JSON format: NDJSON, one object per line, keys: tenant, alias, target,
// created_at (RFC3339). Empty list: json→nothing, text→"(no aliases)" line.
func repoAliasList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repo alias list", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")
	format := fs.String("format", "text", "Output format: text|json")
	fs.SetOutput(stderr)
	if err := fs.Parse(reorderFlagsFirst(args, nil)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: bucketvcs repo alias list --auth-db=<path> <tenant>/<name> [--format=text|json]")
		return 2
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "repo alias list: --format must be text|json (got %q)\n", *format)
		return 2
	}
	tenant, name, err := splitTenantRepo(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()
	aliases, err := s.ListAliases(ctx, tenant, name)
	if err != nil {
		fmt.Fprintf(stderr, "repo alias list: %v\n", err)
		return 1
	}
	if len(aliases) == 0 {
		// JSON format is NDJSON — empty list emits nothing (mirrors policy refs/paths list).
		if *format == "json" {
			return 0
		}
		fmt.Fprintf(stdout, "tenant=%s  repo=%s  (no aliases)\n", tenant, name)
		return 0
	}
	for _, a := range aliases {
		if *format == "json" {
			_ = json.NewEncoder(stdout).Encode(map[string]any{
				"tenant":     a.Tenant,
				"alias":      a.OldName,
				"target":     a.Target,
				"created_at": time.Unix(a.CreatedAt, 0).UTC().Format(time.RFC3339),
			})
			continue
		}
		fmt.Fprintf(stdout, "tenant=%s  alias=%s  target=%s  created=%s\n",
			a.Tenant, a.OldName, a.Target,
			time.Unix(a.CreatedAt, 0).UTC().Format(time.RFC3339))
	}
	return 0
}

// repoAliasRemove implements `bucketvcs repo alias remove --auth-db=<path>
// <tenant>/<old-name>`.
//
// Removes the alias row for (tenant, old-name). Exit 0 if removed, exit 1
// if no such alias (or on store error), exit 2 on usage error.
func repoAliasRemove(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repo alias remove", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")
	fs.SetOutput(stderr)
	if err := fs.Parse(reorderFlagsFirst(args, nil)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: bucketvcs repo alias remove --auth-db=<path> <tenant>/<old-name>")
		return 2
	}
	tenant, oldName, err := splitTenantRepo(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()
	removed, err := s.RemoveAlias(ctx, tenant, oldName)
	if err != nil {
		fmt.Fprintf(stderr, "repo alias remove: %v\n", err)
		return 1
	}
	if !removed {
		fmt.Fprintf(stderr, "no alias %s/%s\n", tenant, oldName)
		return 1
	}
	fmt.Fprintf(stdout, "tenant=%s  alias=%s  removed\n", tenant, oldName)
	return 0
}
