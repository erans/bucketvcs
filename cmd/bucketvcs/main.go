// Command bucketvcs is the entry point for the bucketvcs CLI. M0 ships
// this as a placeholder so that go build ./... succeeds. Real subcommands
// land in later milestones (M3 introduces the Git protocol gateway and
// the first useful CLI surface).
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "bucketvcs: no subcommands available in M0")
	os.Exit(1)
}
