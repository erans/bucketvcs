package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/bucketvcs/bucketvcs/internal/diffharness"
)

func runCatObject(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cat-object", flag.ContinueOnError)
	fs.SetOutput(stderr)
	storeURL := fs.String("store", "", `storage URL, e.g. "localfs:/path"`)
	wantType := fs.Bool("type", false, "print object type")
	wantSize := fs.Bool("size", false, "print object size")
	wantPretty := fs.Bool("pretty", false, "print pretty-printed object content")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *storeURL == "" {
		fmt.Fprintln(stderr, "cat-object: --store is required")
		return 2
	}
	flags := 0
	var mode diffharness.CatObjectMode
	if *wantType {
		flags++
		mode = diffharness.CatType
	}
	if *wantSize {
		flags++
		mode = diffharness.CatSize
	}
	if *wantPretty {
		flags++
		mode = diffharness.CatPretty
	}
	if flags != 1 {
		fmt.Fprintln(stderr, "cat-object: exactly one of --type, --size, --pretty is required")
		return 2
	}
	pos := fs.Args()
	if len(pos) != 3 {
		fmt.Fprintf(stderr, "cat-object: want 3 positional args (tenant repo oid), got %d\n", len(pos))
		return 2
	}
	tenantID, repoID, oidHex := pos[0], pos[1], pos[2]
	store, err := openStore(*storeURL)
	if err != nil {
		fmt.Fprintf(stderr, "cat-object: %v\n", err)
		return 2
	}
	defer closeStore(store)
	out, err := diffharness.CatObject(ctx, store, tenantID, repoID, oidHex, mode)
	if err != nil {
		fmt.Fprintf(stderr, "cat-object: %v\n", err)
		return 1
	}
	if _, err := stdout.Write(out); err != nil {
		fmt.Fprintf(stderr, "cat-object: write stdout: %v\n", err)
		return 1
	}
	return 0
}
