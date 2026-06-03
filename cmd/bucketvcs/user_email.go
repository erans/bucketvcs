package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/bucketvcs/bucketvcs/internal/auth"
)

// userSetEmail implements: bucketvcs user set-email <name> <email>
// An empty <email> ("") clears it.
func userSetEmail(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("user set-email", flag.ContinueOnError)
	fs.SetOutput(stderr)
	authDB := fs.String("auth-db", "", "path to auth.db")
	if err := fs.Parse(reorderFlagsFirst(args, map[string]bool{})); err != nil {
		return 2
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fmt.Fprintln(stderr, "usage: bucketvcs user set-email <name> <email>")
		return 2
	}
	name := fs.Arg(0)
	email := ""
	if fs.NArg() == 2 {
		email = fs.Arg(1)
	}
	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()
	if err := s.SetEmail(ctx, name, email); err != nil {
		switch {
		case errors.Is(err, auth.ErrNoSuchUser):
			fmt.Fprintf(stderr, "no such user %q\n", name)
			return 2
		case errors.Is(err, auth.ErrConflict):
			fmt.Fprintf(stderr, "email %q already in use\n", email)
			return 2
		default:
			fmt.Fprintf(stderr, "set email: %v\n", err)
			return 1
		}
	}
	fmt.Fprintf(stdout, "email set for %s\n", name)
	return 0
}
