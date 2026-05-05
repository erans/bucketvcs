// Package diffharness scaffolds the M2 differential test harness against
// upstream git per spec §40.3. Round-trip and pack-reader oracles run
// over the synthetic corpus from internal/diffharness/fixtures.
package diffharness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/bucketvcs/bucketvcs/internal/objindex"
	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// CatObjectMode selects which cat-object formatter runs.
type CatObjectMode int

const (
	CatType CatObjectMode = iota
	CatSize
	CatPretty
)

// CatObject is the shared implementation of the bucketvcs cat-object
// subcommand. cmd/bucketvcs and the differential harness both call it.
//
// Returns the bytes that would be written to stdout. mode picks the
// formatter; an error is returned for any pipeline failure.
func CatObject(ctx context.Context, store storage.ObjectStore,
	tenant, repoID, oidHex string, mode CatObjectMode) ([]byte, error) {
	r, err := repo.Open(ctx, store, tenant, repoID)
	if err != nil {
		return nil, err
	}
	view, err := r.ReadRoot(ctx)
	if err != nil {
		return nil, err
	}
	var body manifest.Body
	if err := json.Unmarshal(view.Body, &body); err != nil {
		return nil, err
	}
	if body.Indexes.ObjectMap == nil {
		return nil, fmt.Errorf("repo has no object_map")
	}
	mp, err := objindex.OpenWithExpectedHash(ctx, store, body.Indexes.ObjectMap.Key, body.Indexes.ObjectMap.Hash)
	if err != nil {
		return nil, err
	}
	oid, err := pack.ParseOID(oidHex)
	if err != nil {
		return nil, err
	}
	packID, _, ok := mp.Lookup(oid)
	if !ok {
		return nil, fmt.Errorf("oid %s not in object_map", oidHex)
	}
	var pe *manifest.PackEntry
	for i := range body.Packs {
		if body.Packs[i].PackID == packID {
			pe = &body.Packs[i]
			break
		}
	}
	if pe == nil {
		return nil, fmt.Errorf("pack %s missing from manifest", packID)
	}
	pr, err := pack.Open(ctx, store, pe.PackKey, pe.IdxKey)
	if err != nil {
		return nil, err
	}
	defer pr.Close()
	obj, err := pr.Get(ctx, oid)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	switch mode {
	case CatType:
		fmt.Fprintln(&out, obj.Type.String())
	case CatSize:
		fmt.Fprintln(&out, obj.Size)
	case CatPretty:
		switch obj.Type {
		case pack.TypeTree:
			if err := prettyTree(&out, obj.Data); err != nil {
				return nil, err
			}
		default:
			out.Write(obj.Data)
		}
	}
	return out.Bytes(), nil
}

// prettyTree writes a tree object in `git cat-file -p` format. Mode-to-type
// mapping mirrors git: 40000/040000=tree, 160000=commit (gitlink), 120000=blob
// (symlink), all others=blob. Path names with control bytes, tabs, newlines,
// quotes, or high-bit bytes are C-quoted.
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
		var typ string
		switch mode {
		case "40000", "040000":
			typ = "tree"
		case "160000":
			typ = "commit"
		case "120000":
			typ = "blob"
		default:
			typ = "blob"
		}
		paddedMode := mode
		for len(paddedMode) < 6 {
			paddedMode = "0" + paddedMode
		}
		if _, err := fmt.Fprintf(w, "%s %s %s\t%s\n", paddedMode, typ, oid, quotePath(name)); err != nil {
			return err
		}
	}
	return nil
}

// quotePath formats a tree entry pathname the way `git cat-file -p` does.
func quotePath(b []byte) string {
	needsQuote := false
	for _, c := range b {
		if c < 0x20 || c == 0x7f || c == '"' || c == '\\' || c >= 0x80 {
			needsQuote = true
			break
		}
	}
	if !needsQuote {
		return string(b)
	}
	var buf bytes.Buffer
	buf.WriteByte('"')
	for _, c := range b {
		switch c {
		case '\a':
			buf.WriteString(`\a`)
		case '\b':
			buf.WriteString(`\b`)
		case '\t':
			buf.WriteString(`\t`)
		case '\n':
			buf.WriteString(`\n`)
		case '\v':
			buf.WriteString(`\v`)
		case '\f':
			buf.WriteString(`\f`)
		case '\r':
			buf.WriteString(`\r`)
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		default:
			if c < 0x20 || c == 0x7f || c >= 0x80 {
				fmt.Fprintf(&buf, `\%03o`, c)
			} else {
				buf.WriteByte(c)
			}
		}
	}
	buf.WriteByte('"')
	return buf.String()
}
