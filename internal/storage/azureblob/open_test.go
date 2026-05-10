package azureblob_test

import (
	"context"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/storage/azureblob"
)

func TestOpenRejectsBadConfig(t *testing.T) {
	_, err := azureblob.Open(context.Background(), azureblob.Config{})
	if err == nil {
		t.Fatal("Open: want error for empty config")
	}
}

func TestOpenWithConnectionString(t *testing.T) {
	cfg := azureblob.Config{
		Container:        "test",
		ConnectionString: "DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;BlobEndpoint=http://127.0.0.1:10000/devstoreaccount1;",
	}
	a, err := azureblob.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer a.Close()
}
