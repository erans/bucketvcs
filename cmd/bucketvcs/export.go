package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/bucketvcs/bucketvcs/internal/exporter"
	"github.com/bucketvcs/bucketvcs/internal/repo/repoerrs"
)

func runExport(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	storeURL := fs.String("store", "", `storage URL, e.g. "localfs:/path"`)
	noFsck := fs.Bool("no-fsck", false, "skip git fsck after export")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *storeURL == "" {
		fmt.Fprintln(stderr, "export: --store is required")
		return 2
	}
	pos := fs.Args()
	if len(pos) != 3 {
		fmt.Fprintf(stderr, "export: want 3 positional args (tenant repo dst-dir), got %d\n", len(pos))
		return 2
	}
	tenantID, repoID, dst := pos[0], pos[1], pos[2]
	store, err := openStore(*storeURL)
	if err != nil {
		fmt.Fprintf(stderr, "export: %v\n", err)
		return 2
	}
	defer closeStore(store)
	res, err := exporter.Export(ctx, store, exporter.Options{
		Tenant: tenantID, Repo: repoID, DestDir: dst, SkipFsck: *noFsck,
	})
	switch {
	case errors.Is(err, repoerrs.ErrRepoNotFound):
		fmt.Fprintf(stderr, "export: repo %s/%s not found\n", tenantID, repoID)
		return 2
	case errors.Is(err, exporter.ErrDestNotEmpty):
		fmt.Fprintf(stderr, "export: %v\n", err)
		return 2
	case errors.Is(err, exporter.ErrMissingObject):
		fmt.Fprintf(stderr, "export: %v\n", err)
		return 3
	case err != nil:
		fmt.Fprintf(stderr, "export: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "exported %s/%s manifest_version=%d objects=%d fsck=%v\n",
		tenantID, repoID, res.ManifestVersion, res.ObjectCount, res.FsckOK)
	return 0
}
