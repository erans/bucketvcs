package web

import (
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/gitbrowse"
)

func TestGitbrowseSatisfiesContentStore(t *testing.T) {
	var _ ContentStore = (*gitbrowse.Service)(nil)
}
