package deltaindex

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/bucketvcs/bucketvcs/internal/pack"
)

// Encode serializes a Delta to .bvrd bytes. Commits and Packs are sorted by OID
// before emission (deterministic output regardless of input order). RefTips are
// emitted in the caller-supplied order — order may carry create/delete
// sequencing semantics, so Encode does not reorder them.
func Encode(d Delta) ([]byte, error) {
	commits := make([]CommitRecord, len(d.Commits))
	copy(commits, d.Commits)
	sort.Slice(commits, func(i, j int) bool {
		return bytes.Compare(commits[i].OID[:], commits[j].OID[:]) < 0
	})

	packs := make([]pack.OID, len(d.Packs))
	copy(packs, d.Packs)
	sort.Slice(packs, func(i, j int) bool {
		return bytes.Compare(packs[i][:], packs[j][:]) < 0
	})

	var strtab bytes.Buffer
	offsets := make(map[string]uint32, len(d.RefTips))
	for _, r := range d.RefTips {
		if _, seen := offsets[r.RefName]; seen {
			continue
		}
		offsets[r.RefName] = uint32(strtab.Len())
		strtab.WriteString(r.RefName)
		strtab.WriteByte(0)
	}

	var body bytes.Buffer

	body.Write(Magic[:])
	writeU32(&body, VersionCurrent)
	writeU32(&body, uint32(len(commits)))
	writeU32(&body, uint32(len(d.RefTips)))
	writeU32(&body, uint32(len(d.Packs)))
	body.Write(make([]byte, 12))

	for _, c := range commits {
		if len(c.Parents) > 255 {
			return nil, fmt.Errorf("deltaindex: commit has %d parents (>255)", len(c.Parents))
		}
		body.Write(c.OID[:])
		writeU32(&body, c.Generation)
		body.WriteByte(byte(len(c.Parents)))
		for _, p := range c.Parents {
			body.Write(p[:])
		}
	}

	for _, r := range d.RefTips {
		writeU32(&body, offsets[r.RefName])
		body.Write(r.NewOID[:])
		body.Write(r.OldOID[:])
	}

	for _, p := range packs {
		body.Write(p[:])
	}

	writeU32(&body, 0) // reserved trees_blobs_tags
	writeU32(&body, 0) // reserved bitmap

	writeU32(&body, uint32(strtab.Len()))
	body.Write(strtab.Bytes())

	sum := sha256.Sum256(body.Bytes())
	body.Write(sum[:])

	return body.Bytes(), nil
}

func writeU32(w *bytes.Buffer, v uint32) {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	w.Write(b[:])
}
