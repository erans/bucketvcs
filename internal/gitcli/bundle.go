package gitcli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

// BundleCreate invokes `git bundle create <outPath> <ref>` against the
// repository at repoDir. The repo SHOULD be a bare repository
// materialized for the duration of the call (the maintenance code path
// passes a temp bare repo). ref is typically refs/heads/<default-branch>.
//
// On success, outPath contains a Git v2/v3 bundle suitable for delivery
// via the protocol v2 bundle-uri command. On failure, returns the
// combined stdout+stderr in the error so operators can see git's
// complaint verbatim.
func BundleCreate(ctx context.Context, repoDir, outPath, ref string) error {
	if !validRefOrOID(ref) {
		return fmt.Errorf("gitcli: BundleCreate: invalid ref %q", ref)
	}
	bin, err := resolveBinary()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, bin, "-C", repoDir, "bundle", "create", outPath, ref)
	cmd.Env = scrubGitRepoEnv(os.Environ())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gitcli: git bundle create %s %s in %s: %w (%s)", outPath, ref, repoDir, err, out)
	}
	return nil
}
