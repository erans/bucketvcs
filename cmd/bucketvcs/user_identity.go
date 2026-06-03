package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
)

func runUserIdentity(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: bucketvcs user identity <list|remove> ...")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return userIdentityList(ctx, rest, stdout, stderr)
	case "remove":
		return userIdentityRemove(ctx, rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "user identity: unknown subcommand %q\n", sub)
		return 2
	}
}

func userIdentityList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("user identity list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "path to auth.db")
	if err := fs.Parse(reorderFlagsFirst(args, map[string]bool{})); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: bucketvcs user identity list <name>")
		return 2
	}
	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()
	ids, err := s.ListIdentities(ctx, fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "list identities: %v\n", err)
		return 1
	}
	enc := json.NewEncoder(stdout)
	for _, it := range ids {
		_ = enc.Encode(map[string]any{
			"id": it.ID, "provider": it.Provider, "issuer": it.Issuer,
			"subject": it.Subject, "email": it.Email, "created_at": it.CreatedAt,
		})
	}
	return 0
}

func userIdentityRemove(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("user identity remove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "path to auth.db")
	if err := fs.Parse(reorderFlagsFirst(args, map[string]bool{})); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(stderr, "usage: bucketvcs user identity remove <issuer> <subject>")
		return 2
	}
	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()
	if err := s.RemoveIdentity(ctx, fs.Arg(0), fs.Arg(1)); err != nil {
		fmt.Fprintf(stderr, "remove identity: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "identity removed")
	return 0
}
