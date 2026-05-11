// Package deltaindex implements the .bvrd reachability-delta wire
// format (M10 §14.1). Each push produces one immutable .bvrd that
// records new commits + parents + generation numbers, new ref tips,
// and new pack IDs. The file is content-addressed; the storage key
// embeds its SHA-256.
//
// Format (little-endian throughout):
//
//	header (32 bytes):
//	  magic        "BVRD"  (4B)
//	  version      u32     (=1)
//	  n_commits    u32
//	  n_reftips    u32
//	  n_packs      u32
//	  reserved     12B (zero)
//
//	commits (sorted by oid):
//	  oid              20B
//	  generation       u32
//	  n_parents        u8
//	  parents          n_parents * 20B
//
//	reftips:
//	  ref_name_off     u32 (-> strtab offset)
//	  new_oid          20B
//	  old_oid          20B (zero for ref-create)
//
//	packs:
//	  pack_id          20B
//
//	reserved sections (length-prefixed u32, currently zero):
//	  trees_blobs_tags  // Q3=C extension slot for M11
//	  bitmap            // M9.5 extension slot
//
//	strtab (length-prefixed u32 then bytes):
//	  NUL-terminated UTF-8 ref names
//
//	trailer (32 bytes): SHA-256 of preceding bytes
package deltaindex
