// Package oidconst centralises git OID sentinel constants used
// across receivepack, mirror, importer, exporter, refstore,
// reachability, and GC. Previously each consumer declared its own
// local `nullOID`/`nullOIDHex`; this package collapses them to a
// single source of truth.
package oidconst

// NullOIDHex is git's "no object" sentinel — 40 zero hex
// characters — used by the receive-pack wire protocol to mean
// "create" (in OldOID position) or "delete" (in NewOID position),
// and by refstore stage semantics to mean "remove this refname".
const NullOIDHex = "0000000000000000000000000000000000000000"
