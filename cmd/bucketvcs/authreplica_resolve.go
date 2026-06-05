package main

import (
	"errors"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
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

// resolveAuthDBReplica validates and resolves the --auth-db-replica flag.
// Returns (nil, nil) when replication is off.
func resolveAuthDBReplica(replica, storeURL, authDB string, isReplicaServe bool) (*authDBReplicaSpec, error) {
	replica = strings.TrimSpace(replica)
	if replica == "" || replica == "off" {
		return nil, nil
	}
	if sqlitestore.IsNonSQLiteValue(authDB) {
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
