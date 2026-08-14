BINARY := aws-mcp
PKG := github.com/imunhatep/aws-mcp-go
VERSION ?= dev
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS := -X $(PKG)/internal/version.Version=$(VERSION) -X $(PKG)/internal/version.Commit=$(COMMIT)

IMAGE ?= ghcr.io/imunhatep/aws-mcp-go
PLATFORMS := linux/amd64,linux/arm64

.PHONY: build test tidy vendor run image image-multiarch
build:
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/$(BINARY)
test:
	go test ./...
tidy:
	go mod tidy && go mod vendor
vendor:
	go mod vendor
run: build
	./bin/$(BINARY) serve

# Image builds compile from vendor/: awslib comes from a local `replace` that
# points outside the build context, so it has to be vendored in first.
# Build a single-arch image for the local host.
image: vendor
	podman build \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
		-t $(IMAGE):$(VERSION) .

# Build a multi-arch manifest. Override IMAGE=registry/name to tag elsewhere, e.g.:
#   make image-multiarch IMAGE=ghcr.io/someone/aws-mcp-go
image-multiarch: vendor
	podman build --platform $(PLATFORMS) --manifest $(IMAGE):$(VERSION) \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) .
