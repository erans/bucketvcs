package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
)

// openExistingAuthDB opens an auth DB that must already exist — the session
// commands are diagnostics; silently creating an empty DB on a typo'd path
// would read as "no sessions". Returns a non-zero exit code on failure.
func openExistingAuthDB(path string, stderr io.Writer) (*sqlitestore.Store, int) {
	// The no-create existence check only applies to the embedded sqlite
	// file backend; postgres://-style DSNs are passed through (their
	// backends don't create-on-open from a typo).
	if !sqlitestore.IsNonSQLiteValue(path) {
		if _, err := os.Stat(path); err != nil {
			fmt.Fprintf(stderr, "auth-db: %v\n", err)
			return nil, 1
		}
	}
	s, _, err := openAuthDB(path)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return nil, 1
	}
	return s, 0
}

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
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: bucketvcs session list [--auth-db=<path>] [--user=<name>]")
		return 2
	}
	path, err := resolveAuthDB(*authDB, realEnv())
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	s, code := openExistingAuthDB(path, stderr)
	if code != 0 {
		return code
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
	if fs.NArg() != 0 || (*idHash == "") == (*user == "") {
		fmt.Fprintln(stderr, "usage: bucketvcs session revoke [--auth-db=<path>] (--id-hash=<hex> | --user=<name>)")
		return 2
	}
	path, rerr := resolveAuthDB(*authDB, realEnv())
	if rerr != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", rerr)
		return 1
	}
	s, code := openExistingAuthDB(path, stderr)
	if code != 0 {
		return code
	}
	defer s.Close()

	var n int64
	var err error
	var targetUserID, targetUser string
	if *idHash != "" {
		// Best-effort owner attribution: never block the revoke on a lookup
		// failure (including auth.ErrNoSession — the hash may already be gone).
		ownerID, ownerName, oerr := s.SessionOwnerByHash(ctx, *idHash)
		if oerr == nil {
			targetUserID, targetUser = ownerID, ownerName
		} else if !errors.Is(oerr, auth.ErrNoSession) {
			fmt.Fprintf(stderr, "warning: could not resolve session owner for audit attribution: %v\n", oerr)
			// Make the failed lookup explicit in the audit line (vs. "not
			// applicable" on the --user path, where the attrs are omitted).
			targetUser = "(unresolved)"
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
		attrs := []slog.Attr{
			slog.Bool("audit", true),
			slog.String("event", "auth.session.admin_revoked"),
			slog.String("actor", "cli"),
			slog.Int64("count", n),
		}
		if *idHash != "" {
			attrs = append(attrs, slog.String("id_hash", *idHash))
		}
		if targetUserID != "" {
			attrs = append(attrs, slog.String("target_user_id", targetUserID))
		}
		if targetUser != "" {
			attrs = append(attrs, slog.String("target_user", targetUser))
		}
		logger.LogAttrs(ctx, slog.LevelInfo, "auth.session.admin_revoked", attrs...)
	}
	fmt.Fprintf(stdout, "revoked=%d\n", n)
	return 0
}
