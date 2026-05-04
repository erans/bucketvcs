package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// runInit is the body of `bucketvcs init`. Returns the process exit
// code. stdout/stderr are injected for testability.
func runInit(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	storeURL := fs.String("store", "", `Store URL (e.g. "localfs:/var/lib/bucketvcs")`)
	actor := fs.String("actor", defaultActor(), "Actor recorded in the create tx record")
	branch := fs.String("default-branch", "refs/heads/main", "Default branch ref")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *storeURL == "" {
		fmt.Fprintln(stderr, "init: --store is required")
		return 2
	}
	pos := fs.Args()
	if len(pos) != 2 {
		fmt.Fprintf(stderr, "init: want exactly 2 positional args (tenant repo), got %d\n", len(pos))
		return 2
	}
	tenantID, repoID := pos[0], pos[1]

	store, err := openStore(*storeURL)
	if err != nil {
		fmt.Fprintln(stderr, "init:", err)
		return 1
	}
	defer closeStore(store)

	_, err = repo.Create(ctx, store, tenantID, repoID, repo.CreateOptions{
		DefaultBranch: *branch,
		Actor:         *actor,
	})
	if err != nil {
		if errors.Is(err, repo.ErrRepoExists) {
			fmt.Fprintf(stderr, "init: repo %s/%s already exists\n", tenantID, repoID)
			return 1
		}
		fmt.Fprintln(stderr, "init:", err)
		return 1
	}
	fmt.Fprintf(stdout, "created %s/%s\n", tenantID, repoID)
	return 0
}

// closeStore closes the store if it supports Close(). This is needed for
// storage backends like localfs that hold exclusive locks.
func closeStore(store storage.ObjectStore) {
	if closer, ok := store.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
}

func defaultActor() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "unknown"
}
