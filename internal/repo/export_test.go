package repo

// NewTxIDForTest exposes newTxID for the package's external _test.go
// files. The _test.go suffix ensures this is excluded from production
// builds.
func NewTxIDForTest() string { return newTxID() }
