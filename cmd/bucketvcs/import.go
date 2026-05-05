package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/bucketvcs/bucketvcs/internal/importer"
	"github.com/bucketvcs/bucketvcs/internal/repo/repoerrs"
)

func runImport(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	storeURL := fs.String("store", "", `storage URL, e.g. "localfs:/path"`)
	defaultBranch := fs.String("default-branch", "", "default branch ref (overrides source HEAD)")
	actor := fs.String("actor", "", "actor identifier recorded in tx record")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *storeURL == "" {
		fmt.Fprintln(stderr, "import: --store is required")
		return 2
	}
	pos := fs.Args()
	if len(pos) != 3 {
		fmt.Fprintf(stderr, "import: want 3 positional args (source-bare-repo tenant repo), got %d\n", len(pos))
		return 2
	}
	src, tenantID, repoID := pos[0], pos[1], pos[2]
	store, err := openStore(*storeURL)
	if err != nil {
		fmt.Fprintf(stderr, "import: %v\n", err)
		return 2
	}
	defer closeStore(store)
	res, err := importer.Import(ctx, store, importer.Options{
		SourceDir: src, Tenant: tenantID, Repo: repoID,
		Actor: *actor, DefaultBranch: *defaultBranch,
	})
	if err != nil {
		if errors.Is(err, repoerrs.ErrRepoExists) {
			fmt.Fprintf(stderr, "import: repo %s/%s already exists; delete it or import to a different name\n",
				tenantID, repoID)
			return 2
		}
		fmt.Fprintf(stderr, "import: %v\n", err)
		return 1
	}
	fmt.Fprintln(stderr, "fsck source ok")
	fmt.Fprintf(stderr, "pack built %s %d objects\n", res.PackID, res.ObjectCount)
	fmt.Fprintf(stderr, "uploaded pack\n")
	fmt.Fprintf(stderr, "uploaded indexes\n")
	fmt.Fprintf(stderr, "commit %d\n", res.ManifestVersion)
	if _, err := fmt.Fprintf(stdout, "imported %s/%s pack=%s manifest_version=%d refs=%d objects=%d\n",
		tenantID, repoID, res.PackID, res.ManifestVersion, res.RefCount, res.ObjectCount); err != nil {
		fmt.Fprintf(stderr, "import: write stdout: %v\n", err)
		return 1
	}
	return 0
}
