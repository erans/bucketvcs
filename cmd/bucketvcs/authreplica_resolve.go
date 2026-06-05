package main

import (
	"errors"
	"net/url"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/authreplica"
)

// authDBReplicaSpec is the resolved --auth-db-replica configuration.
// UseSystemStore=true means replicate into the --store bucket; otherwise
// StoreURL names a dedicated location (parsed by openStore).
type authDBReplicaSpec struct {
	UseSystemStore bool
	StoreURL       string
	Prefix         string
}

// isNonSQLiteAuthDB mirrors sqlitestore's backend inference: it returns true
// for the same schemes that resolveBackend (internal/auth/sqlitestore/
// backend.go) routes to the postgres or libsql backends. Source of truth is
// resolveBackend's isPostgresValue/isLibsqlValue (both unexported); keep this
// in sync with them. Anything else (bare path, file:, sqlite:) is the embedded
// SQLite backend, which is the only backend authdb replication applies to.
func isNonSQLiteAuthDB(value string) bool {
	u, err := url.Parse(value)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "postgres", "postgresql", "libsql", "http", "https":
		return true
	}
	return false
}

// resolveAuthDBReplica validates and resolves the --auth-db-replica flag.
// Returns (nil, nil) when replication is off.
func resolveAuthDBReplica(replica, storeURL, authDB string, isReplicaServe bool) (*authDBReplicaSpec, error) {
	replica = strings.TrimSpace(replica)
	if replica == "" || replica == "off" {
		return nil, nil
	}
	if isNonSQLiteAuthDB(authDB) {
		return nil, errors.New("--auth-db-replica: replication is for the embedded sqlite backend; libsql/postgres bring their own durability")
	}
	if isReplicaServe {
		return nil, errors.New("--auth-db-replica: not allowed in replica-serve mode (--replica-of); only the primary replicates the authdb")
	}
	if replica == "auto" {
		if storeURL == "" {
			return nil, errors.New("--auth-db-replica=auto requires --store")
		}
		return &authDBReplicaSpec{UseSystemStore: true, Prefix: authreplica.DefaultPrefix}, nil
	}
	return &authDBReplicaSpec{StoreURL: replica, Prefix: authreplica.DefaultPrefix}, nil
}
