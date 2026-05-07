package diffharness

import (
	"context"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/diffharness/fixtures"
	"github.com/bucketvcs/bucketvcs/internal/gateway"
	"github.com/bucketvcs/bucketvcs/internal/importer"
)

// cloneAuthOracleSkip lists fixtures that legitimately cannot pass the
// auth-wrapped clone-equivalence oracle. Mirrors cloneOracleSkip — by
// construction the auth path should accept exactly the same fixtures the
// anonymous path accepts, so any divergence here is a real bug.
var cloneAuthOracleSkip = map[string]string{}

// TestOracle_CloneEquivalenceWithAuth verifies that the byte-level result
// of `git clone --mirror` against the gateway is identical regardless of
// whether the client clones anonymously (public_read repo, no creds) or
// authenticated (private repo, admin token via Basic auth).
//
// The point of the oracle is to confirm that the auth middleware is
// *transparent* to the wire protocol: once a request is admitted, the
// upload-pack response bytes — and therefore the reachable object set in
// the resulting clone — must match the fixture exactly, identical to what
// the no-auth M3 path produced.
//
// Implementation: for each fixture, run two clones in sequence against two
// independently-configured gateways:
//
//  1. Anonymous: AuthStore registers the repo as public_read=true, clone
//     is unauthenticated. (Identical configuration to TestOracle_CloneEquivalence.)
//  2. Authenticated: AuthStore registers the repo as private + admin user;
//     clone embeds Basic-auth user:token in the URL.
//
// Both clones are compared against the source fixture (the canonical
// equivalence relation) AND against each other (object-set identity). If
// either comparison fails, the auth path has perturbed the bytes.
func TestOracle_CloneEquivalenceWithAuth(t *testing.T) {
	skipIfNoGit(t)
	for name, build := range fixtures.Registry {
		name, build := name, build
		t.Run(name, func(t *testing.T) {
			if reason, skip := cloneAuthOracleSkip[name]; skip {
				t.Skip(reason)
			}
			t.Parallel()
			workDir := t.TempDir()
			srcDir := filepath.Join(workDir, "src")
			fx := build(t, srcDir)
			gitFsck(t, srcDir)

			tenant := "fx"
			repoID := name
			defaultBranch := ""
			if len(fx.Refs) == 0 {
				defaultBranch = "refs/heads/main"
			}

			// --- run #1: anonymous against public_read=true repo --------
			anonOIDs, anonRefs := cloneAndCollect(t, workDir, "anon",
				srcDir, tenant, repoID, defaultBranch,
				newDiffharnessAuthStore(t, tenant, repoID),
				"", "")

			// --- run #2: authenticated against private repo ------------
			authStore, adminUser, adminToken := newDiffharnessAuthStoreWithAdminToken(t, tenant, repoID)
			authOIDs, authRefs := cloneAndCollect(t, workDir, "auth",
				srcDir, tenant, repoID, defaultBranch,
				authStore, adminUser, adminToken)

			// Empty fixture: both runs return nil OID lists — trivially equal.
			if len(fx.Refs) == 0 {
				if anonOIDs != nil || authOIDs != nil {
					t.Fatalf("empty fixture produced unexpected OIDs.\nanon=%v\nauth=%v", anonOIDs, authOIDs)
				}
				return
			}

			// Auth and anon results must match each other byte-for-byte.
			if !equalRefs(anonRefs, authRefs) {
				t.Fatalf("refs differ between anon and auth clones.\nanon=%v\nauth=%v", anonRefs, authRefs)
			}
			if !equalOIDLists(anonOIDs, authOIDs) {
				t.Fatalf("reachable OIDs differ between anon and auth clones.\nanon=%v\nauth=%v", anonOIDs, authOIDs)
			}
			// And they must both match the source fixture (the existing
			// CloneEquivalence oracle already proves this for the anon
			// path; checking it here too keeps this test self-contained).
			srcRefs := gitShowRef(t, srcDir)
			if !equalRefs(srcRefs, authRefs) {
				t.Fatalf("auth clone refs diverge from source.\nsrc=%v\nauth=%v", srcRefs, authRefs)
			}
			srcOIDs := gitRevListAllObjects(t, srcDir)
			if !equalOIDLists(srcOIDs, authOIDs) {
				t.Fatalf("auth clone OIDs diverge from source.\nsrc=%v\nauth=%v", srcOIDs, authOIDs)
			}
		})
	}
}

// cloneAndCollect imports srcDir into a fresh object store, starts a
// gateway with the provided AuthStore, runs `git clone --mirror` (with
// optional Basic-auth credentials embedded in the URL), and returns the
// reachable OID list and ref map of the resulting clone. It validates the
// clone with `git fsck`.
//
// label is used to namespace the clone destination directory inside
// workDir so multiple invocations within one test don't collide.
//
// When user/token are both empty the URL is left unauthenticated.
func cloneAndCollect(t *testing.T, workDir, label, srcDir, tenant, repoID, defaultBranch string,
	authStore auth.Store, user, token string) (oids []string, refs map[string]string) {
	t.Helper()
	store := newTestStore(t)
	if _, err := importer.Import(context.Background(), store, importer.Options{
		SourceDir:     srcDir,
		Tenant:        tenant,
		Repo:          repoID,
		Actor:         "harness",
		DefaultBranch: defaultBranch,
	}); err != nil {
		t.Fatalf("[%s] Import: %v", label, err)
	}
	srv, err := gateway.NewServer(store, gateway.Options{
		MirrorDir: t.TempDir(),
		Version:   "test",
		AuthStore: authStore,
	})
	if err != nil {
		t.Fatalf("[%s] NewServer: %v", label, err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	cloneURL := ts.URL + "/" + tenant + "/" + repoID + ".git"
	if user != "" || token != "" {
		u, perr := url.Parse(cloneURL)
		if perr != nil {
			t.Fatalf("[%s] parse URL: %v", label, perr)
		}
		u.User = url.UserPassword(user, token)
		cloneURL = u.String()
	}

	dstDir := filepath.Join(workDir, label+"-clone.git")
	cmd := exec.Command("git", "clone", "--mirror",
		"-c", "protocol.version=2",
		cloneURL, dstDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("[%s] git clone --mirror: %v\n%s", label, err, out)
	}

	// Empty fixture: --mirror produced a bare repo with no refs; nothing
	// to fsck or list.
	if defaultBranch != "" {
		// Defensive: defaultBranch is only set when fx.Refs is empty.
		return nil, nil
	}
	gitFsck(t, dstDir)
	return gitRevListAllObjects(t, dstDir), gitShowRef(t, dstDir)
}
