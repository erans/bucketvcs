package main

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
)

// envLookup is a small abstraction over os.Getenv so resolveAuthDB and
// resolveHostKey are testable without mutating process state.
type envLookup struct {
	BUCKETVCS_AUTH_DB      string
	BUCKETVCS_SSH_HOST_KEY string
	XDG_STATE_HOME         string
	HOME                   string
}

func realEnv() *envLookup {
	return &envLookup{
		BUCKETVCS_AUTH_DB:      os.Getenv("BUCKETVCS_AUTH_DB"),
		BUCKETVCS_SSH_HOST_KEY: os.Getenv("BUCKETVCS_SSH_HOST_KEY"),
		XDG_STATE_HOME:         os.Getenv("XDG_STATE_HOME"),
		HOME:                   os.Getenv("HOME"),
	}
}

// resolveAuthDB returns the auth DB path using the resolution order from
// the M4 spec §7.3:
//  1. flag (if non-empty)
//  2. BUCKETVCS_AUTH_DB
//  3. $XDG_STATE_HOME/bucketvcs/bucketvcs.db
//  4. $HOME/.local/state/bucketvcs/bucketvcs.db
func resolveAuthDB(flag string, env *envLookup) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if env.BUCKETVCS_AUTH_DB != "" {
		return env.BUCKETVCS_AUTH_DB, nil
	}
	if env.XDG_STATE_HOME != "" {
		return filepath.Join(env.XDG_STATE_HOME, "bucketvcs", "bucketvcs.db"), nil
	}
	if env.HOME != "" {
		return filepath.Join(env.HOME, ".local", "state", "bucketvcs", "bucketvcs.db"), nil
	}
	return "", errors.New("auth-db: cannot resolve default path; set --auth-db, BUCKETVCS_AUTH_DB, XDG_STATE_HOME, or HOME")
}

// openAuthDB resolves the path, ensures the parent directory exists, and
// returns an opened sqlitestore. Caller must Close.
func openAuthDB(flag string, opts ...sqlitestore.Option) (*sqlitestore.Store, string, error) {
	path, err := resolveAuthDB(flag, realEnv())
	if err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, "", err
	}
	s, err := sqlitestore.Open(path, opts...)
	if err != nil {
		return nil, "", err
	}
	return s, path, nil
}
