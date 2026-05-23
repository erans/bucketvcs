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
		fmt.Fprintln(stderr, "usage: bucketvcs token <create|list|revoke|rotate>")
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
	case "rotate":
		return tokenRotate(ctx, rest, stdout, stderr)
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
	// fs.Func lets us distinguish "--scopes omitted" (legacy mode, with
	// warning) from "--scopes=" explicitly empty (usage error, exit 2 per
	// spec §9). fs.String would conflate the two by storing "" in both
	// cases. We capture only that the flag was seen + its raw value; the
	// validation/parse logic stays below alongside the other post-parse
	// checks.
	scopesPassed := false
	scopesValue := ""
	fs.Func("scopes",
		"Token scopes (csv|all|repo:*|lfs:*); omit for legacy (full user permissions)",
		func(s string) error {
			scopesPassed = true
			scopesValue = s
			return nil
		})
	fs.SetOutput(stderr)
	if err := fs.Parse(reorderFlagsFirst(args, nil)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: bucketvcs token create <user> [--expires <duration>] [--label <text>] [--scopes <csv>]")
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
	scopes := auth.ScopeLegacy
	switch {
	case scopesPassed && scopesValue == "":
		// Spec §9 failure-modes: --scopes= with an empty value is a
		// usage error (a request for "no scope at all"). Operators who
		// want legacy / unscoped tokens must omit the flag.
		fmt.Fprintln(stderr,
			"invalid --scopes: must be non-empty if specified; omit the flag for legacy")
		return 2
	case scopesPassed:
		parsed, err := auth.ParseScopes(scopesValue)
		if err != nil {
			fmt.Fprintf(stderr, "invalid --scopes: %v\n", err)
			return 2
		}
		scopes = parsed
	default:
		fmt.Fprintln(stderr,
			"warning: no --scopes set; token has full user permissions (legacy mode)")
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
	if err := s.CreateToken(ctx, id, u.ID, hash, *label, expPtr, scopes); err != nil {
		fmt.Fprintf(stderr, "create token: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "id=%s\ntoken=%s\nscopes=%s\n", id, tok, auth.FormatScopes(scopes))
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
	fmt.Fprintln(stdout, "id\tlabel\tcreated\texpires\trevoked\tlast-used\tscopes")
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
		fmt.Fprintf(stdout, "%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
			r.ID, r.Label, r.CreatedAt, exp, rev, last, auth.FormatScopes(r.Scopes))
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

// tokenRotate mints a fresh secret for an existing token id and persists the
// new PHC-encoded argon2id hash via Store.RotateToken. The id, label, scopes,
// and expiry are preserved (verified at the Store layer in M17 Task 2). The
// new plaintext token is printed once on stdout; it is the only time the
// caller will see it.
func tokenRotate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("token rotate", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")
	id := fs.String("id", "", "token id (required)")
	fs.SetOutput(stderr)
	if err := fs.Parse(reorderFlagsFirst(args, nil)); err != nil {
		return 2
	}
	if *id == "" {
		fmt.Fprintln(stderr, "usage: bucketvcs token rotate --id <token-id> [--auth-db <path>]")
		return 2
	}
	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()
	// Reuse GenerateToken to mint id+secret, but discard the new id — we
	// only want a fresh secret stitched onto the existing token id, so the
	// plaintext we emit is `bvts_<existing-id>_<new-secret>`. This matches
	// the wire format Parse/auth checks expect.
	_, _, secret, err := auth.GenerateToken()
	if err != nil {
		fmt.Fprintf(stderr, "generate: %v\n", err)
		return 1
	}
	hash, err := auth.HashSecret(secret)
	if err != nil {
		fmt.Fprintf(stderr, "hash: %v\n", err)
		return 1
	}
	if err := s.RotateToken(ctx, *id, hash); err != nil {
		if errors.Is(err, auth.ErrNoSuchToken) {
			fmt.Fprintf(stderr, "token rotate: %v\n", err)
			return 2
		}
		fmt.Fprintf(stderr, "token rotate: %v\n", err)
		return 1
	}
	// M17 Task 6: emit auth.token.rotated audit. RotateToken already
	// succeeded; a GetTokenByID failure here only degrades the audit
	// payload (empty user_id) — it does not affect the operation.
	tok, _ := s.GetTokenByID(ctx, *id)
	userID := ""
	if tok != nil {
		userID = tok.UserID
	}
	auth.EmitTokenRotated(ctx, nil, *id, userID, cliActor())
	// Re-use auth.AssembleToken so the wire format (prefix + separators)
	// remains a single source of truth shared with auth.GenerateToken /
	// auth.ParseToken — if it ever changes, this rotate path picks it up
	// for free instead of silently drifting.
	plaintext := auth.AssembleToken(*id, secret)
	fmt.Fprintf(stdout, "id=%s rotated\ntoken=%s\n", *id, plaintext)
	fmt.Fprintln(stderr, "(this is the only time the new token will be shown; copy it now)")
	return 0
}
