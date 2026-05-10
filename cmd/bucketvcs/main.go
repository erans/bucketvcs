// Command bucketvcs is the bucketvcs CLI entry point. M2 wires five
// subcommands: `init`, `inspect-manifest`, `import`, `export`, and
// `cat-object`. Subcommand surface expands per-milestone (M3 adds the
// protocol gateway, M8 adds gc, etc.).
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
	case "import":
		return runImport(ctx, rest, stdout, stderr)
	case "export":
		return runExport(ctx, rest, stdout, stderr)
	case "cat-object":
		return runCatObject(ctx, rest, stdout, stderr)
	case "serve":
		return runServe(ctx, rest, stdout, stderr)
	case "user":
		return runUser(ctx, rest, stdout, stderr)
	case "token":
		return runToken(ctx, rest, stdout, stderr)
	case "repo":
		return runRepo(ctx, rest, stdout, stderr)
	case "ssh":
		return runSSH(ctx, rest, stdout, stderr)
	case "gc":
		return runGC(ctx, rest, stdout, stderr)
	case "maintenance":
		return runMaintenance(ctx, rest, stdout, stderr)
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
  import             Round-trip a bare git repo into bucketvcs storage
  export             Materialize a bare git repo from bucketvcs storage
  cat-object         Read a Git object from a bucketvcs repo
  serve              Run the HTTP smart-Git gateway
  ssh                SSH subcommands (fingerprint)
  user               Manage users (add/list/disable/enable/delete)
  token              Manage tokens (create/list/revoke)
  repo               Manage repository registry and permissions
  gc                 Garbage-collect orphan and unreachable storage
  maintenance        Run repack maintenance against repos

Run "bucketvcs <subcommand> --help" for subcommand flags.
`)
}
