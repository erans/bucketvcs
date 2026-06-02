package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
)

// reorderFlagsFirst moves "-flag" / "--flag" tokens (and their values for
// non-bool flags) to the front of the arg list so Go's stdlib flag.Parse
// — which stops at the first non-flag — accepts CLI styles like
// `bucketvcs user add alice --admin`.
//
// A literal `--` terminator is preserved at the boundary between the
// reordered flags and the positionals so that flag.Parse stops there and
// treats any dash-prefixed positional (e.g. a user name like `-foo`,
// allowed by validName) as a literal argument rather than an unknown flag.
func reorderFlagsFirst(args []string, boolFlags map[string]bool) []string {
	flagsArgs := make([]string, 0, len(args))
	positional := make([]string, 0, len(args))
	sawTerminator := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			sawTerminator = true
			positional = append(positional, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			flagsArgs = append(flagsArgs, a)
			// `--flag=value` form: value is in the same token, no
			// extra consumption required.
			if strings.Contains(a, "=") {
				continue
			}
			name := strings.TrimLeft(a, "-")
			if !boolFlags[name] && i+1 < len(args) {
				i++
				flagsArgs = append(flagsArgs, args[i])
			}
			continue
		}
		positional = append(positional, a)
	}
	out := flagsArgs
	if sawTerminator {
		out = append(out, "--")
	}
	return append(out, positional...)
}

func runUser(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: bucketvcs user <add|list|disable|enable|delete|key|set-password> [flags]")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "add":
		return userAdd(ctx, rest, stdout, stderr)
	case "list":
		return userList(ctx, rest, stdout, stderr)
	case "disable":
		return userSetDisabled(ctx, rest, stdout, stderr, true)
	case "enable":
		return userSetDisabled(ctx, rest, stdout, stderr, false)
	case "delete":
		return userDelete(ctx, rest, stdout, stderr)
	case "key":
		return runUserKey(ctx, rest, stdout, stderr)
	case "set-password":
		return userSetPassword(ctx, rest, stdout, stderr, os.Stdin)
	default:
		fmt.Fprintf(stderr, "user: unknown subcommand %q\n", sub)
		return 2
	}
}

func userAdd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("user add", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")
	admin := fs.Bool("admin", false, "create as admin")
	fs.SetOutput(stderr)
	if err := fs.Parse(reorderFlagsFirst(args, map[string]bool{"admin": true})); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: bucketvcs user add <name> [--admin]")
		return 2
	}
	name := fs.Arg(0)
	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()
	if _, err := s.CreateUser(ctx, name, *admin); err != nil {
		if errors.Is(err, auth.ErrConflict) {
			fmt.Fprintf(stderr, "user %q already exists\n", name)
			return 2
		}
		fmt.Fprintf(stderr, "create user: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "created user %s\n", name)
	return 0
}

func userList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("user list", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")
	fs.SetOutput(stderr)
	if err := fs.Parse(reorderFlagsFirst(args, nil)); err != nil {
		return 2
	}
	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()
	users, err := s.ListUsers(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "list: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "name\tadmin\tdisabled\tcreated")
	for _, u := range users {
		adm := "no"
		if u.IsAdmin {
			adm = "admin"
		}
		dis := "no"
		if u.DisabledAt != nil {
			dis = "yes"
		}
		fmt.Fprintf(stdout, "%s\t%s\t%s\t%d\n", u.Name, adm, dis, u.CreatedAt)
	}
	return 0
}

func userSetDisabled(ctx context.Context, args []string, stdout, stderr io.Writer, disabled bool) int {
	fs := flag.NewFlagSet("user enable/disable", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")
	fs.SetOutput(stderr)
	if err := fs.Parse(reorderFlagsFirst(args, nil)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: bucketvcs user {enable|disable} <name>")
		return 2
	}
	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()
	if err := s.SetUserDisabled(ctx, fs.Arg(0), disabled); err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	return 0
}

func userDelete(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("user delete", flag.ContinueOnError)
	authDB := fs.String("auth-db", "", "path to auth.db")
	fs.SetOutput(stderr)
	if err := fs.Parse(reorderFlagsFirst(args, nil)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: bucketvcs user delete <name>")
		return 2
	}
	s, _, err := openAuthDB(*authDB)
	if err != nil {
		fmt.Fprintf(stderr, "auth-db: %v\n", err)
		return 1
	}
	defer s.Close()
	if err := s.DeleteUser(ctx, fs.Arg(0)); err != nil {
		if errors.Is(err, sqlitestore.ErrLastAdmin) {
			fmt.Fprintln(stderr, "refusing: would remove last admin")
			return 1
		}
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	return 0
}
