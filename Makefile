GO ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/gm2211/grove/internal/cli.Version=$(VERSION)

.PHONY: build test lint ui clean images

build:
	$(GO) build -ldflags "$(LDFLAGS)" -o bin/grove ./cmd/grove

test:
	$(GO) vet ./...
	$(GO) test ./...

ui:
	cd ui && npm ci && npm run build

# Builds both Tart worker VM images locally. Mac-only (Tart needs Virtualization.framework) — see
# docs/IMAGES.md. Does not push; run `tart push` yourself once you've eyeballed the result.
images:
	cd images/macos-worker && packer init . && packer validate . && packer build .
	cd images/linux-worker && packer init . && packer validate . && packer build .

clean:
	rm -rf bin dist ui/dist internal/server/dist
