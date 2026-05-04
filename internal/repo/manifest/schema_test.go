package manifest_test

import (
	"errors"
	"testing"

	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

func TestSchemaGate_Accepts(t *testing.T) {
	cases := []manifest.RootHeader{
		{SchemaVersion: 1, MinReaderVersion: "0.0.0"},
		{SchemaVersion: 1, MinReaderVersion: manifest.SupportedReaderVersion},
		{SchemaVersion: 1, MinReaderVersion: ""}, // empty == accept
	}
	for _, h := range cases {
		if err := manifest.SchemaGate(h); err != nil {
			t.Errorf("SchemaGate(%+v) want nil, got %v", h, err)
		}
	}
}

func TestSchemaGate_RejectsFutureSchemaVersion(t *testing.T) {
	h := manifest.RootHeader{SchemaVersion: 2, MinReaderVersion: "0.1.0"}
	if err := manifest.SchemaGate(h); !errors.Is(err, repo.ErrUnsupportedSchema) {
		t.Errorf("want ErrUnsupportedSchema, got %v", err)
	}
}

func TestSchemaGate_RejectsFutureMinReader(t *testing.T) {
	h := manifest.RootHeader{SchemaVersion: 1, MinReaderVersion: "999.0.0"}
	if err := manifest.SchemaGate(h); !errors.Is(err, repo.ErrUnsupportedSchema) {
		t.Errorf("want ErrUnsupportedSchema, got %v", err)
	}
}

func TestSchemaGate_RejectsZeroSchemaVersion(t *testing.T) {
	h := manifest.RootHeader{SchemaVersion: 0, MinReaderVersion: "0.1.0"}
	if err := manifest.SchemaGate(h); !errors.Is(err, repo.ErrUnsupportedSchema) {
		t.Errorf("want ErrUnsupportedSchema, got %v", err)
	}
}
