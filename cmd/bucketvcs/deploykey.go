package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"golang.org/x/crypto/ssh"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/sshd"
)

// runRepoDeployKey dispatches `bucketvcs repo deploy-key <subcommand>`.
func runRepoDeployKey(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: bucketvcs repo deploy-key <add|list|revoke>")
		return 2
	}
	switch args[0] {
	case "add":
		return repoDeployKeyAdd(ctx, args[1:], stdout, stderr)
	case "list":
		return repoDeployKeyList(ctx, args[1:], stdout, stderr)
	case "revoke":
		return repoDeployKeyRevoke(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "repo deploy-key: unknown subcommand %q\n", args[0])
		return 2
	}
}

func repoDeployKeyAdd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repo deploy-key add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	label := fs.String("label", "", "Operator-supplied label for the deploy key")
	useStdin := fs.Bool("stdin", false, "Read pubkey from stdin instead of a file")
	if err := fs.Parse(reorderFlagsFirst(args, map[string]bool{"stdin": true})); err != nil {
		return 2
	}
	rest := fs.Args()

	var tenantRepo, pubkeyPath, permStr string
	if *useStdin {
		if len(rest) != 2 {
			fmt.Fprintln(stderr, "usage: bucketvcs repo deploy-key add <tenant>/<repo> --stdin <read|write> [--label TEXT]")
			return 2
		}
		tenantRepo, permStr = rest[0], rest[1]
	} else {
		if len(rest) != 3 {
			fmt.Fprintln(stderr, "usage: bucketvcs repo deploy-key add <tenant>/<repo> <pubkey-file> <read|write> [--label TEXT]")
			return 2
		}
		tenantRepo, pubkeyPath, permStr = rest[0], rest[1], rest[2]
	}

	// Validate tenant/repo format.
	tenant, repo, err := splitTenantRepo(tenantRepo)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}

	// Validate permission — deploy keys cannot have admin perm.
	var perm auth.Perm
	switch permStr {
	case "read":
		perm = auth.PermRead
	case "write":
		perm = auth.PermWrite
	case "admin":
		fmt.Fprintln(stderr, "repo deploy-key add: deploy keys cannot have 'admin' permission")
		return 2
	default:
		fmt.Fprintf(stderr, "repo deploy-key add: invalid permission %q (must be read|write)\n", permStr)
		return 2
	}

	// Read public key bytes.
	var pubBytes []byte
	if *useStdin {
		pubBytes, err = io.ReadAll(bufio.NewReader(os.Stdin))
	} else {
		pubBytes, err = os.ReadFile(pubkeyPath)
	}
	if err != nil {
		fmt.Fprintf(stderr, "repo deploy-key add: read pubkey: %v\n", err)
		return 1
	}

	parsedKey, _, _, _, err := ssh.ParseAuthorizedKey(pubBytes)
	if err != nil {
		fmt.Fprintf(stderr, "repo deploy-key add: not an OpenSSH public key: %v\n", err)
		return 1
	}
	fp := sshd.SHA256Fingerprint(parsedKey)
	keyType := parsedKey.Type()
	wireBytes := parsedKey.Marshal()

	db, _, err := openAuthDB("")
	if err != nil {
		fmt.Fprintf(stderr, "repo deploy-key add: open db: %v\n", err)
		return 1
	}
	defer db.Close()

	// Verify the repo is registered.
	if _, err := db.GetRepoFlags(ctx, tenant, repo); err != nil {
		if errors.Is(err, auth.ErrNoSuchRepo) {
			fmt.Fprintf(stderr, "repo deploy-key add: repo %q not registered\n", tenantRepo)
			return 1
		}
		fmt.Fprintf(stderr, "repo deploy-key add: lookup repo: %v\n", err)
		return 1
	}

	keyID, err := auth.GenerateSSHKeyID()
	if err != nil {
		fmt.Fprintf(stderr, "repo deploy-key add: generate id: %v\n", err)
		return 1
	}

	err = db.AddSSHKey(ctx, auth.SSHKey{
		ID:          keyID,
		Fingerprint: fp,
		PublicKey:   wireBytes,
		KeyType:     keyType,
		Label:       *label,
		ScopeTenant: tenant,
		ScopeRepo:   repo,
		ScopePerm:   perm,
	})
	if err != nil {
		if errors.Is(err, auth.ErrDuplicateFingerprint) {
			fmt.Fprintln(stderr, "repo deploy-key add: fingerprint already registered")
			return 1
		}
		fmt.Fprintf(stderr, "repo deploy-key add: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "added deploy key %s (%s %s) %s for %s/%s\n",
		keyID, fp, keyType, permStr, tenant, repo)
	return 0
}

func repoDeployKeyList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repo deploy-key list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "Output JSON")
	if err := fs.Parse(reorderFlagsFirst(args, map[string]bool{"json": true})); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fmt.Fprintln(stderr, "usage: bucketvcs repo deploy-key list <tenant>/<repo> [--json]")
		return 2
	}

	tenant, repo, err := splitTenantRepo(rest[0])
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}

	db, _, err := openAuthDB("")
	if err != nil {
		fmt.Fprintf(stderr, "repo deploy-key list: open db: %v\n", err)
		return 1
	}
	defer db.Close()

	keys, err := db.ListSSHKeysForRepo(ctx, tenant, repo)
	if err != nil {
		fmt.Fprintf(stderr, "repo deploy-key list: %v\n", err)
		return 1
	}

	if *asJSON {
		return emitKeyJSON(stdout, keys)
	}
	// Text columns: id | label | type | fingerprint | perm | created | last-used | revoked
	fmt.Fprintf(stdout, "%-30s  %-20s  %-15s  %-50s  %-6s  %-20s  %-20s  %-20s\n",
		"ID", "LABEL", "TYPE", "FINGERPRINT", "PERM", "CREATED", "LAST_USED", "REVOKED")
	for _, k := range keys {
		fmt.Fprintf(stdout, "%-30s  %-20s  %-15s  %-50s  %-6s  %-20s  %-20s  %-20s\n",
			k.ID, k.Label, k.KeyType, k.Fingerprint,
			auth.PermToText(k.ScopePerm),
			unixTimeStr(k.CreatedAt),
			unixTimeStr(k.LastUsedAt),
			unixTimeStr(k.RevokedAt),
		)
	}
	return 0
}

func repoDeployKeyRevoke(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("repo deploy-key revoke", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fmt.Fprintln(stderr, "usage: bucketvcs repo deploy-key revoke <key-id-or-prefix>")
		return 2
	}
	keyIDOrPrefix := rest[0]

	db, _, err := openAuthDB("")
	if err != nil {
		fmt.Fprintf(stderr, "repo deploy-key revoke: open db: %v\n", err)
		return 1
	}
	defer db.Close()

	if err := db.RevokeSSHKey(ctx, keyIDOrPrefix); err != nil {
		if errors.Is(err, auth.ErrNoSuchKey) {
			fmt.Fprintf(stderr, "repo deploy-key revoke: no such key %q\n", keyIDOrPrefix)
			return 1
		}
		if errors.Is(err, auth.ErrConflict) {
			fmt.Fprintf(stderr, "repo deploy-key revoke: ambiguous prefix %q\n", keyIDOrPrefix)
			return 1
		}
		fmt.Fprintf(stderr, "repo deploy-key revoke: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "revoked %s\n", keyIDOrPrefix)
	return 0
}
