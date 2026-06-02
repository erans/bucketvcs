package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// userSetPassword implements: bucketvcs user set-password <name> --password-stdin
// The stdin reader is injected for testability (production passes os.Stdin).
func userSetPassword(ctx context.Context, args []string, stdout, stderr io.Writer, stdin io.Reader) int {
	fs := flag.NewFlagSet("user set-password", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "path to auth.db")
	fromStdin := fs.Bool("password-stdin", false, "read the password from stdin (recommended)")
	if err := fs.Parse(reorderFlagsFirst(args, map[string]bool{"password-stdin": true})); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: bucketvcs user set-password <name> --password-stdin")
		return 2
	}
	name := fs.Arg(0)

	if !*fromStdin {
		fmt.Fprintln(stderr, "set-password: --password-stdin is required (pass the password on stdin)")
		return 2
	}
	pw, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		fmt.Fprintf(stderr, "read password: %v\n", err)
		return 1
	}
	pw = strings.TrimRight(pw, "\r\n")
	if pw == "" {
		fmt.Fprintln(stderr, "set-password: empty password")
		return 2
	}

	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()

	if err := s.SetPassword(ctx, name, pw); err != nil {
		if errors.Is(err, auth.ErrNoSuchUser) {
			fmt.Fprintf(stderr, "no such user %q\n", name)
			return 2
		}
		fmt.Fprintf(stderr, "set password: %v\n", err)
		return 1
	}
	auth.EmitPasswordSet(ctx, nil, name) // audit
	fmt.Fprintf(stdout, "password set for %s\n", name)
	return 0
}
