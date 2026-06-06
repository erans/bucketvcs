package main

import (
	"errors"
	"path/filepath"
)

// resolveSpoolDir returns the local log-spool directory using the same
// resolution order as resolveAuthDB:
//  1. flag (--log-spool-dir, if non-empty)
//  2. BUCKETVCS_LOG_SPOOL_DIR
//  3. $XDG_STATE_HOME/bucketvcs/log-spool
//  4. $HOME/.local/state/bucketvcs/log-spool
//
// The directory is created by shiplog.New (MkdirAll); this helper only
// derives the path.
func resolveSpoolDir(flag string, env *envLookup) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if env.BUCKETVCS_LOG_SPOOL_DIR != "" {
		return env.BUCKETVCS_LOG_SPOOL_DIR, nil
	}
	if env.XDG_STATE_HOME != "" {
		return filepath.Join(env.XDG_STATE_HOME, "bucketvcs", "log-spool"), nil
	}
	if env.HOME != "" {
		return filepath.Join(env.HOME, ".local", "state", "bucketvcs", "log-spool"), nil
	}
	return "", errors.New("log-spool-dir: cannot resolve default path; set --log-spool-dir, BUCKETVCS_LOG_SPOOL_DIR, XDG_STATE_HOME, or HOME")
}
