package main

import (
	"fmt"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/storage"
	"github.com/bucketvcs/bucketvcs/internal/storage/localfs"
)

// parseStoreURL parses a --store value into (scheme, scheme-specific
// path). M1 supports only "localfs:<path>"; cloud schemes ("s3:",
// "gcs:", "r2:", "azureblob:") are recognized but rejected with an
// explanatory error pointing at the milestone that will add them.
func parseStoreURL(s string) (scheme, path string, err error) {
	colon := strings.IndexByte(s, ':')
	if colon <= 0 {
		return "", "", fmt.Errorf(`--store: missing scheme; want "localfs:<path>"`)
	}
	scheme = s[:colon]
	path = s[colon+1:]
	if path == "" {
		return "", "", fmt.Errorf(`--store: %q scheme requires a non-empty path (got %q)`, scheme, s)
	}
	switch scheme {
	case "localfs":
		return scheme, path, nil
	case "s3", "gcs", "r2", "azureblob":
		return "", "", fmt.Errorf(`--store: scheme %q is reserved; cloud adapters land at M5/M7`, scheme)
	default:
		return "", "", fmt.Errorf(`--store: unknown scheme %q; want "localfs:<path>"`, scheme)
	}
}

// openStore parses the --store URL and returns a constructed
// ObjectStore. Caller is responsible for releasing it via closeStore on
// shutdown — localfs holds a process-wide lockfile that must be released.
// closeStore is defined in init.go and asserts io.Closer at runtime.
func openStore(url string) (storage.ObjectStore, error) {
	scheme, path, err := parseStoreURL(url)
	if err != nil {
		return nil, err
	}
	switch scheme {
	case "localfs":
		s, err := localfs.Open(path)
		if err != nil {
			return nil, fmt.Errorf("localfs: %w", err)
		}
		return s, nil
	default:
		return nil, fmt.Errorf("unreachable: scheme %q passed parseStoreURL but openStore has no constructor", scheme)
	}
}
