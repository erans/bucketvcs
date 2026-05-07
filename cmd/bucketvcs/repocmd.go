package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

func runRepo(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: bucketvcs repo <register|grant|revoke|public|list>")
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
	default:
		fmt.Fprintf(stderr, "repo: unknown subcommand %q\n", sub)
		return 2
	}
}

func splitTenantRepo(s string) (string, string, error) {
	i := strings.IndexByte(s, '/')
	if i <= 0 || i == len(s)-1 {
		return "", "", fmt.Errorf("expected tenant/repo, got %q", s)
	}
	return s[:i], s[i+1:], nil
}

func repoRegister(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repo register", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")
	noInit := fs.Bool("no-init", false, "skip M1 bucket init (registry only)")
	storeURL := fs.String("store", "", `Store URL for M1 init (e.g. "localfs:/var/lib/bucketvcs"); ignored with --no-init`)
	fs.SetOutput(stderr)
	if err := fs.Parse(reorderFlagsFirst(args, map[string]bool{"no-init": true})); err != nil {
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
	if err := s.RegisterRepo(ctx, tenant, repo); err != nil {
		fmt.Fprintf(stderr, "register: %v\n", err)
		return 1
	}
	return 0
}

func repoGrant(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repo grant", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")
	fs.SetOutput(stderr)
	if err := fs.Parse(reorderFlagsFirst(args, nil)); err != nil {
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
	fs.SetOutput(stderr)
	if err := fs.Parse(reorderFlagsFirst(args, nil)); err != nil {
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
	fs.SetOutput(stderr)
	if err := fs.Parse(reorderFlagsFirst(args, nil)); err != nil {
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
