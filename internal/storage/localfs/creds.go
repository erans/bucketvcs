package localfs

// ApplyCredsJSON is a no-op for localfs — it has no credentials.
func ApplyCredsJSON(raw []byte) error { return nil }
