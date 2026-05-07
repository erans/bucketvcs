package diffharness

import (
	"context"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/diffharness/fixtures"
	"github.com/bucketvcs/bucketvcs/internal/exporter"
	"github.com/bucketvcs/bucketvcs/internal/gateway"
	"github.com/bucketvcs/bucketvcs/internal/importer"
)

// pushOracleSkip lists fixtures that legitimately cannot pass the push-
// equivalence oracle as currently formulated. Each entry must explain why.
var pushOracleSkip = map[string]string{
	// The gateway rejects refs/replace/* writes via receive-pack as a M3
	// invariant (see internal/gateway/receive_pack.go).
	"replace_ref": "refs/replace/* writes are blocked by the gateway as an M3 invariant",

	// symref_head's HEAD points at a non-default branch. The seeded empty
	// repo's manifest defaults to refs/heads/main; receive-pack does not
	// update HEAD. After push the exporter's HEAD will reflect the seeded
	// default, not the source's. Covered by Task 23 (HEAD-symref oracle).
	"symref_head": "HEAD propagation through push is covered by Task 23; receive-pack does not advertise HEAD updates",
}

// TestOracle_PushEquivalence verifies that for each fixture, `git push --mirror`
// to a freshly-seeded gateway-backed repo, followed by an export, produces a
// bare repo whose reachable objects match the fixture.
func TestOracle_PushEquivalence(t *testing.T) {
	skipIfNoGit(t)
	for name, build := range fixtures.Registry {
		name, build := name, build
		t.Run(name, func(t *testing.T) {
			if reason, skip := pushOracleSkip[name]; skip {
				t.Skip(reason)
			}
			t.Parallel()
			workDir := t.TempDir()
			srcDir := filepath.Join(workDir, "src")
			fx := build(t, srcDir)
			gitFsck(t, srcDir)

			// Empty fixture: nothing to push; trivial case covered by M2 round-trip.
			if len(fx.Refs) == 0 {
				t.Skip("empty fixture has no refs to push")
			}

			store := newTestStore(t)
			tenant := "fx"
			repoID := name

			// Seed an empty bare repo via importer so the manifest exists.
			emptyBare := filepath.Join(workDir, "empty.git")
			if out, err := exec.Command("git", "init", "--bare", emptyBare).CombinedOutput(); err != nil {
				t.Fatalf("git init --bare: %v\n%s", err, out)
			}
			if _, err := importer.Import(context.Background(), store, importer.Options{
				SourceDir:     emptyBare,
				Tenant:        tenant,
				Repo:          repoID,
				Actor:         "harness",
				DefaultBranch: "refs/heads/main",
			}); err != nil {
				t.Fatalf("seed Import: %v", err)
			}

			authStore, adminUser, adminToken := newDiffharnessAuthStoreWithAdminToken(t, tenant, repoID)
			srv, err := gateway.NewServer(store, gateway.Options{
				MirrorDir: t.TempDir(),
				Version:   "test",
				AuthStore: authStore,
			})
			if err != nil {
				t.Fatalf("NewServer: %v", err)
			}
			t.Cleanup(func() { _ = srv.Close() })
			ts := httptest.NewServer(srv)
			t.Cleanup(ts.Close)

			// Embed Basic-auth credentials in the push URL: `git push`
			// runs credential helpers only on a 401 challenge, so for the
			// non-interactive harness path we precompute the URL with
			// user:token. The token grants admin (no Grant call needed —
			// admins short-circuit Decide).
			pushURL := withDiffharnessBasicAuth(ts.URL+"/"+tenant+"/"+repoID+".git", adminUser, adminToken)
			pushCmd := exec.Command("git", "-C", srcDir, "push", "--mirror", pushURL)
			if out, err := pushCmd.CombinedOutput(); err != nil {
				t.Fatalf("git push --mirror: %v\n%s", err, out)
			}

			dstDir := filepath.Join(workDir, "exported.git")
			if _, err := exporter.Export(context.Background(), store, exporter.Options{
				Tenant:  tenant,
				Repo:    repoID,
				DestDir: dstDir,
			}); err != nil {
				t.Fatalf("exporter.Export: %v", err)
			}
			gitFsck(t, dstDir)

			srcRefs := gitShowRef(t, srcDir)
			dstRefs := gitShowRef(t, dstDir)
			if !equalRefs(srcRefs, dstRefs) {
				t.Fatalf("refs differ.\nsrc=%v\ndst=%v", srcRefs, dstRefs)
			}
			srcOIDs := gitRevListAllObjects(t, srcDir)
			dstOIDs := gitRevListAllObjects(t, dstDir)
			if !equalOIDLists(srcOIDs, dstOIDs) {
				t.Fatalf("reachable OIDs differ.\nsrc=%v\ndst=%v", srcOIDs, dstOIDs)
			}
			for _, oid := range srcOIDs {
				got := gitCatFilePretty(t, dstDir, oid)
				want := gitCatFilePretty(t, srcDir, oid)
				ensureBytesEqual(t, "cat-file -p "+oid, got, want)
			}
		})
	}
}

// withDiffharnessBasicAuth rewrites a remote URL to embed user:pass
// credentials. `git push` only invokes credential helpers on a 401
// challenge, so the harness embeds creds directly to keep the path
// non-interactive.
func withDiffharnessBasicAuth(remote, user, pass string) string {
	u, err := url.Parse(remote)
	if err != nil {
		return remote
	}
	u.User = url.UserPassword(user, pass)
	return u.String()
}
