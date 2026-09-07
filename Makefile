GO ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/gm2211/grove/internal/cli.Version=$(VERSION)

.PHONY: build test lint ui clean

build:
	$(GO) build -ldflags "$(LDFLAGS)" -o bin/grove ./cmd/grove

test:
	$(GO) vet ./...
	$(GO) test ./...

ui:
	cd ui && npm ci && npm run build

clean:
	rm -rf bin dist ui/dist internal/server/dist
