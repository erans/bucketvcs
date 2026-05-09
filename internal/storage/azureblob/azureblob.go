package azureblob

import (
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"

	bvstorage "github.com/bucketvcs/bucketvcs/internal/storage"
)

// AzureBlob is the Azure Blob Storage storage.ObjectStore implementation.
type AzureBlob struct {
	cfg       Config
	service   *azblob.Client
	container *container.Client
}

// placeholder until Task 2.2 lands config.go
type Config struct{}

// var _ bvstorage.ObjectStore = (*AzureBlob)(nil)

// Capabilities reports the Azure adapter capabilities. MultipartMinPartSize
// reflects Azure's ~100 KiB practical block-blob minimum; MultipartMaxParts
// is Azure's documented 50000-block ceiling per blob; MaxObjectSize is the
// modern Azure 190.7 TiB block-blob max (4000 MiB block * 50000 blocks).
func (a *AzureBlob) Capabilities() bvstorage.Capabilities {
	return bvstorage.Capabilities{
		SignedURLs:           true,
		StrongList:           true,
		MultipartMinPartSize: 100 << 10,
		MultipartMaxParts:    50000,
		MaxObjectSize:        190 << 40, // 190 TiB
	}
}
