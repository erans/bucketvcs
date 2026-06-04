package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// runUserKey dispatches `bucketvcs user key <subcommand>`.
func runUserKey(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: bucketvcs user key <add|list|revoke>")
		return 2
	}
	switch args[0] {
	case "add":
		return userKeyAdd(ctx, args[1:], stdout, stderr)
	case "list":
		return userKeyList(ctx, args[1:], stdout, stderr)
	case "revoke":
		return userKeyRevoke(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "user key: unknown subcommand %q\n", args[0])
		return 2
	}
}

func userKeyAdd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("user key add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	label := fs.String("label", "", "Operator-supplied label for the key")
	useStdin := fs.Bool("stdin", false, "Read pubkey from stdin instead of a file")
	if err := fs.Parse(reorderFlagsFirst(args, map[string]bool{"stdin": true})); err != nil {
		return 2
	}
	rest := fs.Args()
	var userName, pubkeyPath string
	if *useStdin {
		if len(rest) != 1 {
			fmt.Fprintln(stderr, "usage: bucketvcs user key add <user> --stdin [--label TEXT]")
			return 2
		}
		userName = rest[0]
	} else {
		if len(rest) != 2 {
			fmt.Fprintln(stderr, "usage: bucketvcs user key add <user> <pubkey-file> [--label TEXT]")
			return 2
		}
		userName, pubkeyPath = rest[0], rest[1]
	}

	var pubBytes []byte
	var err error
	if *useStdin {
		pubBytes, err = io.ReadAll(bufio.NewReader(os.Stdin))
	} else {
		pubBytes, err = os.ReadFile(pubkeyPath)
	}
	if err != nil {
		fmt.Fprintf(stderr, "user key add: read pubkey: %v\n", err)
		return 1
	}

	db, _, err := openAuthDB("")
	if err != nil {
		fmt.Fprintf(stderr, "user key add: open db: %v\n", err)
		return 1
	}
	defer db.Close()

	user, err := db.GetUserByName(ctx, userName)
	if err != nil {
		if errors.Is(err, auth.ErrNoSuchUser) {
			fmt.Fprintf(stderr, "user key add: no such user %q\n", userName)
			return 1
		}
		fmt.Fprintf(stderr, "user key add: lookup user: %v\n", err)
		return 1
	}

	k, err := auth.BuildUserSSHKey(pubBytes, user.ID, *label)
	if err != nil {
		fmt.Fprintf(stderr, "user key add: not an OpenSSH public key: %v\n", err)
		return 1
	}

	err = db.AddSSHKey(ctx, k)
	if err != nil {
		if errors.Is(err, auth.ErrDuplicateFingerprint) {
			fmt.Fprintf(stderr, "user key add: fingerprint already registered\n")
			return 1
		}
		fmt.Fprintf(stderr, "user key add: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "added key %s (%s %s) for %s\n", k.ID, k.Fingerprint, k.KeyType, userName)
	return 0
}

func userKeyList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("user key list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "Output JSON")
	if err := fs.Parse(reorderFlagsFirst(args, map[string]bool{"json": true})); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fmt.Fprintln(stderr, "usage: bucketvcs user key list <user> [--json]")
		return 2
	}
	userName := rest[0]

	db, _, err := openAuthDB("")
	if err != nil {
		fmt.Fprintf(stderr, "user key list: open db: %v\n", err)
		return 1
	}
	defer db.Close()

	user, err := db.GetUserByName(ctx, userName)
	if err != nil {
		if errors.Is(err, auth.ErrNoSuchUser) {
			fmt.Fprintf(stderr, "user key list: no such user %q\n", userName)
			return 1
		}
		fmt.Fprintf(stderr, "user key list: %v\n", err)
		return 1
	}

	keys, err := db.ListSSHKeysForUser(ctx, user.ID)
	if err != nil {
		fmt.Fprintf(stderr, "user key list: %v\n", err)
		return 1
	}

	if *asJSON {
		return emitKeyJSON(stdout, keys)
	}
	// Text columns: id | label | type | fingerprint | created | last-used | revoked
	fmt.Fprintf(stdout, "%-30s  %-20s  %-15s  %-50s  %-20s  %-20s  %-20s\n",
		"ID", "LABEL", "TYPE", "FINGERPRINT", "CREATED", "LAST_USED", "REVOKED")
	for _, k := range keys {
		fmt.Fprintf(stdout, "%-30s  %-20s  %-15s  %-50s  %-20s  %-20s  %-20s\n",
			k.ID, k.Label, k.KeyType, k.Fingerprint,
			unixTimeStr(k.CreatedAt),
			unixTimeStr(k.LastUsedAt),
			unixTimeStr(k.RevokedAt),
		)
	}
	return 0
}

func userKeyRevoke(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("user key revoke", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fmt.Fprintln(stderr, "usage: bucketvcs user key revoke <key-id-or-prefix>")
		return 2
	}
	keyIDOrPrefix := rest[0]

	db, _, err := openAuthDB("")
	if err != nil {
		fmt.Fprintf(stderr, "user key revoke: open db: %v\n", err)
		return 1
	}
	defer db.Close()

	if err := db.RevokeSSHKey(ctx, keyIDOrPrefix); err != nil {
		if errors.Is(err, auth.ErrNoSuchKey) {
			fmt.Fprintf(stderr, "user key revoke: no such key %q\n", keyIDOrPrefix)
			return 1
		}
		if errors.Is(err, auth.ErrConflict) {
			fmt.Fprintf(stderr, "user key revoke: ambiguous prefix %q\n", keyIDOrPrefix)
			return 1
		}
		fmt.Fprintf(stderr, "user key revoke: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "revoked %s\n", keyIDOrPrefix)
	return 0
}

// emitKeyJSON writes the keys as a JSON array.
func emitKeyJSON(stdout io.Writer, keys []auth.SSHKey) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(keys); err != nil {
		return 1
	}
	return 0
}

// unixTimeStr formats a unix-seconds field for human-readable output.
// Returns empty string for zero (meaning "not set").
func unixTimeStr(sec int64) string {
	if sec == 0 {
		return ""
	}
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}
