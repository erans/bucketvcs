# bucketvcs — local development Makefile.
# Cross-platform release artifacts (all 5 targets + .deb/.rpm) are produced by
# GoReleaser, not here; see .goreleaser.yaml. This Makefile is for fast host
# builds during development.

# Version stamped into the binary, matching the release pipeline's ldflag.
# Falls back to "dev" outside a git checkout.
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.buildVersion=$(VERSION)

BIN := bin/bucketvcs

.PHONY: build clean

## build: compile the bucketvcs binary for the host platform into ./bin
build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/bucketvcs

## clean: remove local build output (bin/) and GoReleaser output (dist/)
clean:
	rm -rf bin dist
