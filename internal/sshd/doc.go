// Package sshd implements the bucketvcs SSH gateway. It wraps
// golang.org/x/crypto/ssh, parses Git exec commands, authenticates via
// public key against an auth.Store, and dispatches to the gitproto
// engines.
//
// The package has no HTTP, no SQL, and no manifest imports. Authentication
// goes through the auth.Store seam; protocol work goes through gitproto.
package sshd
