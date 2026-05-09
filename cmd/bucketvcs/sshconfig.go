package main

import (
	"errors"
	"path/filepath"
)

// resolveHostKey returns the SSH host key path. Resolution order:
//  1. flag value (the --ssh-host-key)
//  2. $BUCKETVCS_SSH_HOST_KEY
//  3. $XDG_STATE_HOME/bucketvcs/ssh_host_ed25519_key
//  4. $HOME/.local/state/bucketvcs/ssh_host_ed25519_key
func resolveHostKey(flagValue string, env *envLookup) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if v := env.BUCKETVCS_SSH_HOST_KEY; v != "" {
		return v, nil
	}
	if env.XDG_STATE_HOME != "" {
		return filepath.Join(env.XDG_STATE_HOME, "bucketvcs", "ssh_host_ed25519_key"), nil
	}
	if env.HOME != "" {
		return filepath.Join(env.HOME, ".local", "state", "bucketvcs", "ssh_host_ed25519_key"), nil
	}
	return "", errors.New("ssh-host-key: cannot resolve default path; set --ssh-host-key, BUCKETVCS_SSH_HOST_KEY, XDG_STATE_HOME, or HOME")
}
