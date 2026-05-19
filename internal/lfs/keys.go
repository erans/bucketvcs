package lfs

// RepoLFSPrefix returns the object-store key prefix for a repo's LFS
// area: tenants/<tenant>/repos/<repo>/lfs/objects/. This is the
// canonical layout per the M13 spec §4 and the single source of truth
// for the prefix; the gateway's lfs.Store factory and the
// ProxiedObjectHandler both consume it.
func RepoLFSPrefix(tenant, repo string) string {
	return "tenants/" + tenant + "/repos/" + repo + "/lfs/objects/"
}
