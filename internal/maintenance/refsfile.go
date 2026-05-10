package maintenance

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// writePackedRefs writes <bareDir>/packed-refs from the manifest's ref
// map. Lines are sorted by ref name for determinism (so two materialize
// runs over the same manifest produce byte-identical output). Standard
// packed-refs header.
func writePackedRefs(bareDir string, refs map[string]string) error {
	names := make([]string, 0, len(refs))
	for k := range refs {
		names = append(names, k)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("# pack-refs with: peeled fully-peeled sorted\n")
	for _, n := range names {
		fmt.Fprintf(&b, "%s %s\n", refs[n], n)
	}
	return os.WriteFile(filepath.Join(bareDir, "packed-refs"), []byte(b.String()), 0o644)
}

// writeHEAD writes <bareDir>/HEAD as a symbolic ref to
// refs/heads/<defaultBranch>.
func writeHEAD(bareDir, defaultBranch string) error {
	if defaultBranch == "" {
		return fmt.Errorf("writeHEAD: defaultBranch is empty")
	}
	body := fmt.Sprintf("ref: refs/heads/%s\n", defaultBranch)
	return os.WriteFile(filepath.Join(bareDir, "HEAD"), []byte(body), 0o644)
}

// writeMinimalConfig writes the smallest [core] block git needs to
// recognize <bareDir> as a bare repo for fsck and pack-objects.
func writeMinimalConfig(bareDir string) error {
	body := "[core]\n\trepositoryformatversion = 0\n\tbare = true\n"
	return os.WriteFile(filepath.Join(bareDir, "config"), []byte(body), 0o644)
}
