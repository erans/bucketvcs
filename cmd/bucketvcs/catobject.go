package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/bucketvcs/bucketvcs/internal/objindex"
	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
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
	if *wantType {
		flags++
	}
	if *wantSize {
		flags++
	}
	if *wantPretty {
		flags++
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
	r, err := repo.Open(ctx, store, tenantID, repoID)
	if err != nil {
		fmt.Fprintf(stderr, "cat-object: %v\n", err)
		return 2
	}
	view, err := r.ReadRoot(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "cat-object: ReadRoot: %v\n", err)
		return 1
	}
	var body manifest.Body
	if err := json.Unmarshal(view.Body, &body); err != nil {
		fmt.Fprintf(stderr, "cat-object: unmarshal body: %v\n", err)
		return 1
	}
	if body.Indexes.ObjectMap == nil {
		fmt.Fprintln(stderr, "cat-object: repo has no object_map index")
		return 3
	}
	mp, err := objindex.Open(ctx, store, body.Indexes.ObjectMap.Key)
	if err != nil {
		fmt.Fprintf(stderr, "cat-object: open object_map: %v\n", err)
		return 3
	}
	oid, err := pack.ParseOID(oidHex)
	if err != nil {
		fmt.Fprintf(stderr, "cat-object: bad oid %q: %v\n", oidHex, err)
		return 2
	}
	packID, _, ok := mp.Lookup(oid)
	if !ok {
		fmt.Fprintf(stderr, "cat-object: oid %s not in object_map\n", oidHex)
		return 2
	}
	var pe *manifest.PackEntry
	for i := range body.Packs {
		if body.Packs[i].PackID == packID {
			pe = &body.Packs[i]
			break
		}
	}
	if pe == nil {
		fmt.Fprintf(stderr, "cat-object: pack %s referenced by object_map missing from manifest\n", packID)
		return 3
	}
	pr, err := pack.Open(ctx, store, pe.PackKey, pe.IdxKey)
	if err != nil {
		fmt.Fprintf(stderr, "cat-object: open pack: %v\n", err)
		return 3
	}
	defer pr.Close()
	obj, err := pr.Get(ctx, oid)
	if err != nil {
		fmt.Fprintf(stderr, "cat-object: get: %v\n", err)
		return 1
	}
	switch {
	case *wantType:
		fmt.Fprintln(stdout, obj.Type.String())
	case *wantSize:
		fmt.Fprintln(stdout, obj.Size)
	case *wantPretty:
		switch obj.Type {
		case pack.TypeTree:
			if err := prettyTree(stdout, obj.Data); err != nil {
				fmt.Fprintf(stderr, "cat-object: pretty tree: %v\n", err)
				return 1
			}
		default:
			if _, err := stdout.Write(obj.Data); err != nil {
				return 1
			}
		}
	}
	return 0
}

// prettyTree writes a tree object in `git cat-file -p` format:
//
//	<mode> SP <type> SP <oid> TAB <name> NL
func prettyTree(w io.Writer, data []byte) error {
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		if sp < 0 {
			return fmt.Errorf("malformed tree entry: no space")
		}
		mode := string(data[:sp])
		data = data[sp+1:]
		nul := bytes.IndexByte(data, 0)
		if nul < 0 {
			return fmt.Errorf("malformed tree entry: no NUL")
		}
		name := data[:nul]
		data = data[nul+1:]
		if len(data) < 20 {
			return fmt.Errorf("malformed tree entry: short oid")
		}
		var oid pack.OID
		copy(oid[:], data[:20])
		data = data[20:]
		typ := "blob"
		if mode == "40000" || mode == "040000" {
			typ = "tree"
		}
		paddedMode := mode
		for len(paddedMode) < 6 {
			paddedMode = "0" + paddedMode
		}
		fmt.Fprintf(w, "%s %s %s\t%s\n", paddedMode, typ, oid, name)
	}
	return nil
}
