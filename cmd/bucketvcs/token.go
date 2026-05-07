package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
)

func runToken(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: bucketvcs token <create|list|revoke>")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "create":
		return tokenCreate(ctx, rest, stdout, stderr)
	case "list":
		return tokenList(ctx, rest, stdout, stderr)
	case "revoke":
		return tokenRevoke(ctx, rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "token: unknown subcommand %q\n", sub)
		return 2
	}
}

func tokenCreate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("token create", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")
	expires := fs.String("expires", "", "expiration duration, e.g. 90d, 24h; empty means never")
	label := fs.String("label", "", "human-readable label")
	fs.SetOutput(stderr)
	if err := fs.Parse(reorderFlagsFirst(args, nil)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: bucketvcs token create <user> [--expires <duration>] [--label <text>]")
		return 2
	}
	user := fs.Arg(0)
	var expPtr *int64
	if *expires != "" {
		d, err := parseDuration(*expires)
		if err != nil {
			fmt.Fprintf(stderr, "invalid --expires: %v\n", err)
			return 2
		}
		exp := time.Now().Add(d).Unix()
		expPtr = &exp
	}
	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()
	u, err := s.GetUserByName(ctx, user)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	tok, id, secret, err := auth.GenerateToken()
	if err != nil {
		fmt.Fprintf(stderr, "generate: %v\n", err)
		return 1
	}
	hash, err := auth.HashSecret(secret)
	if err != nil {
		fmt.Fprintf(stderr, "hash: %v\n", err)
		return 1
	}
	if err := s.CreateToken(ctx, id, u.ID, hash, *label, expPtr); err != nil {
		fmt.Fprintf(stderr, "create token: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, tok)
	fmt.Fprintln(stderr, "(this is the only time the full token will be shown; copy it now)")
	return 0
}

// parseDuration accepts the standard Go time.ParseDuration syntax with the
// addition of a "d" suffix meaning days.
func parseDuration(s string) (time.Duration, error) {
	if len(s) > 1 && s[len(s)-1] == 'd' {
		var n int
		if _, err := fmt.Sscanf(s, "%dd", &n); err != nil {
			return 0, err
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

func tokenList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("token list", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")
	fs.SetOutput(stderr)
	if err := fs.Parse(reorderFlagsFirst(args, nil)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: bucketvcs token list <user>")
		return 2
	}
	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()
	rows, err := s.ListTokensForUser(ctx, fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "id\tlabel\tcreated\texpires\trevoked\tlast-used")
	for _, r := range rows {
		exp := "-"
		if r.ExpiresAt != nil {
			exp = fmt.Sprintf("%d", *r.ExpiresAt)
		}
		rev := "-"
		if r.RevokedAt != nil {
			rev = fmt.Sprintf("%d", *r.RevokedAt)
		}
		last := "-"
		if r.LastUsedAt != nil {
			last = fmt.Sprintf("%d", *r.LastUsedAt)
		}
		fmt.Fprintf(stdout, "%s\t%s\t%d\t%s\t%s\t%s\n", r.ID, r.Label, r.CreatedAt, exp, rev, last)
	}
	return 0
}

func tokenRevoke(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("token revoke", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")
	fs.SetOutput(stderr)
	if err := fs.Parse(reorderFlagsFirst(args, nil)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: bucketvcs token revoke <token-id-or-prefix>")
		return 2
	}
	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()
	id, err := s.ResolveTokenIDPrefix(ctx, fs.Arg(0))
	if err != nil {
		if errors.Is(err, sqlitestore.ErrAmbiguousPrefix) {
			fmt.Fprintln(stderr, "ambiguous token id prefix; supply more characters")
			return 2
		}
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	if err := s.RevokeToken(ctx, id); err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	return 0
}
