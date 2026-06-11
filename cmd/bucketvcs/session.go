package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
)

func runSession(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: bucketvcs session <list|revoke>")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return sessionList(ctx, rest, stdout, stderr)
	case "revoke":
		return sessionRevoke(ctx, rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "session: unknown subcommand %q\n", sub)
		return 2
	}
}

// sessionList prints every web session as NDJSON, newest-first by last_seen
// (the store's ordering). The CLI is the escape hatch past the admin page's
// 500-row display cap.
func sessionList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("session list", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")
	user := fs.String("user", "", "only sessions belonging to this user name")
	fs.SetOutput(stderr)
	if err := fs.Parse(reorderFlagsFirst(args, nil)); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *authDB == "" {
		fmt.Fprintln(stderr, "usage: bucketvcs session list --auth-db=<path> [--user=<name>]")
		return 2
	}
	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()
	rows, _, err := s.ListAllSessions(ctx, 0)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	enc := json.NewEncoder(stdout)
	for _, row := range rows {
		if *user != "" && row.UserName != *user {
			continue
		}
		_ = enc.Encode(map[string]any{
			"id_hash":    row.IDHash,
			"user_id":    row.UserID,
			"user":       row.UserName,
			"provider":   row.Provider,
			"created_at": row.CreatedAt,
			"expires_at": row.ExpiresAt,
			"last_seen":  row.LastSeen,
		})
	}
	return 0
}

// sessionRevoke is implemented in Task 5; stub so the group compiles.
func sessionRevoke(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fmt.Fprintln(stderr, "session revoke: not implemented")
	return 2
}
