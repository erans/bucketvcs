package refstore

// MarshalAndHash marshals refs using the production shard-content
// encoding and returns (bytes, hash, err). Used by the conformance
// suite, the reshard CLI, and observability tooling that needs to
// compute what a shard object's key would be for a given ref set
// without going through the full ShardedRefStore write path.
//
// The returned hash is the canonical lowercase-hex sha256 string that
// appears in manifest.RefShard.Hash and feeds keys.Repo.RefShardKey.
//
// In practice the error is unreachable for refnames that are valid
// UTF-8 (json.Marshal over map[string]string only fails on invalid
// UTF-8 in keys), but the error return surfaces it cleanly for any
// caller that wants to handle it instead of crashing.
func MarshalAndHash(refs map[string]string) ([]byte, string, error) {
	b, err := marshalShardContent(refs)
	if err != nil {
		return nil, "", err
	}
	return b, hashShardContent(b), nil
}
