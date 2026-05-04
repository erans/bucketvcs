// Command bucketvcs is the bucketvcs CLI entry point. M1 wires two
// subcommands: `init` and `inspect-manifest`. Subcommand surface
// expands per-milestone (M3 adds the protocol gateway, M8 adds gc, etc.).
package main

import (
	"context"
	"fmt"
	"io"
	"os"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches to a subcommand. Exit codes:
//
//	0  success
//	1  general error (store, IO, ...)
//	2  usage / not found
//	3  schema-gate refusal
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "init":
		return runInit(ctx, rest, stdout, stderr)
	case "inspect-manifest":
		return runInspect(ctx, rest, stdout, stderr)
	case "-h", "--help", "help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "bucketvcs: unknown subcommand %q\n", sub)
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `Usage: bucketvcs <subcommand> [flags] [args]

Subcommands:
  init               Create a new repo
  inspect-manifest   Print summary of the root manifest

Run "bucketvcs <subcommand> --help" for subcommand flags.
`)
}
