package gitbrowse

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/browsemodel"
)

func TestOpenMirror_ReturnsBareDir(t *testing.T) {
	svc, tenant, repo, _ := fixture(t)
	m, release, err := svc.openMirror(context.Background(), tenant, repo)
	if err != nil {
		t.Fatalf("openMirror: %v", err)
	}
	defer release()
	if m.BareDir() == "" {
		t.Fatal("empty bare dir")
	}
}

func TestOpenMirror_TimeoutIsWarming(t *testing.T) {
	svc, tenant, repo, _ := fixture(t)
	svc.timeout = 1 * time.Nanosecond // force the cold-open deadline to blow
	_, _, err := svc.openMirror(context.Background(), tenant, repo)
	if !errors.Is(err, browsemodel.ErrWarming) {
		t.Fatalf("want ErrWarming, got %v", err)
	}
}
