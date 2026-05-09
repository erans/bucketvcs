package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"golang.org/x/crypto/ssh"

	"github.com/bucketvcs/bucketvcs/internal/sshd"
)

// runSSH dispatches `bucketvcs ssh <subcommand>`.
func runSSH(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: bucketvcs ssh <subcommand>")
		fmt.Fprintln(stderr, "subcommands: fingerprint")
		return 2
	}
	switch args[0] {
	case "fingerprint":
		return runSSHFingerprint(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "ssh: unknown subcommand %q\n", args[0])
		return 2
	}
}

func runSSHFingerprint(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ssh fingerprint", flag.ContinueOnError)
	fs.SetOutput(stderr)
	hostKey := fs.String("ssh-host-key", "", "Path to host key (default: $XDG_STATE_HOME/bucketvcs/ssh_host_ed25519_key)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	path, err := resolveHostKey(*hostKey, realEnv())
	if err != nil {
		fmt.Fprintf(stderr, "ssh fingerprint: %v\n", err)
		return 2
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(stderr, "ssh fingerprint: read %s: %v\n", path, err)
		return 1
	}
	signer, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		fmt.Fprintf(stderr, "ssh fingerprint: parse %s: %v\n", path, err)
		return 1
	}
	fp := sshd.SHA256Fingerprint(signer.PublicKey())
	fmt.Fprintf(stdout, "%s bucketvcs host key (%s)\n", fp, signer.PublicKey().Type())
	return 0
}
