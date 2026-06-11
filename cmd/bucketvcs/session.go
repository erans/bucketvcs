package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"

	"github.com/bucketvcs/bucketvcs/internal/auth"
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
		if err := enc.Encode(map[string]any{
			"id_hash":    row.IDHash,
			"user_id":    row.UserID,
			"user":       row.UserName,
			"provider":   row.Provider,
			"created_at": row.CreatedAt,
			"expires_at": row.ExpiresAt,
			"last_seen":  row.LastSeen,
		}); err != nil {
			fmt.Fprintf(stderr, "encode: %v\n", err)
			return 1
		}
	}
	return 0
}

// sessionRevoke deletes sessions by stored id hash or by owning user name.
// Idempotent: deleting an already-gone session prints revoked=0 and exits 0.
// The audit trail for CLI revocations is stderr-only (CLI emitters are not
// shipped; see the observability operator guide).
func sessionRevoke(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("session revoke", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")
	idHash := fs.String("id-hash", "", "stored session id hash (from session list / the admin page)")
	user := fs.String("user", "", "revoke ALL sessions of this user name")
	fs.SetOutput(stderr)
	if err := fs.Parse(reorderFlagsFirst(args, nil)); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *authDB == "" || (*idHash == "") == (*user == "") {
		fmt.Fprintln(stderr, "usage: bucketvcs session revoke --auth-db=<path> (--id-hash=<hex> | --user=<name>)")
		return 2
	}
	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()

	var n int64
	var targetUserID, targetUser string
	if *idHash != "" {
		// Best-effort owner attribution: never block the revoke on a lookup
		// failure (including auth.ErrNoSession — the hash may already be gone).
		ownerID, ownerName, oerr := s.SessionOwnerByHash(ctx, *idHash)
		if oerr == nil {
			targetUserID, targetUser = ownerID, ownerName
		} else if !errors.Is(oerr, auth.ErrNoSession) {
			fmt.Fprintf(stderr, "warning: could not resolve session owner for audit attribution: %v\n", oerr)
		}
		n, err = s.DeleteSessionByHash(ctx, *idHash)
	} else {
		u, uerr := s.GetUserByName(ctx, *user)
		if uerr != nil {
			fmt.Fprintf(stderr, "%v\n", uerr)
			return 1
		}
		targetUserID, targetUser = u.ID, u.Name
		n, err = s.DeleteSessionsForUser(ctx, u.ID, "")
	}
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	// stderr-only audit line (CLI events are not shipped). Like the web
	// handlers, a no-op revoke (count=0) emits no audit event.
	if n > 0 {
		logger := slog.New(slog.NewTextHandler(stderr, nil))
		logger.LogAttrs(ctx, slog.LevelInfo, "auth.session.admin_revoked",
			slog.Bool("audit", true),
			slog.String("event", "auth.session.admin_revoked"),
			slog.String("actor", "cli"),
			slog.String("id_hash", *idHash),
			slog.String("target_user_id", targetUserID),
			slog.String("target_user", targetUser),
			slog.Int64("count", n),
		)
	}
	fmt.Fprintf(stdout, "revoked=%d\n", n)
	return 0
}
