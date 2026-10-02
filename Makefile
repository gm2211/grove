GO ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/gm2211/grove/internal/cli.Version=$(VERSION)

.PHONY: build test lint ui clean images images-macos images-macos-xcode images-macos-host-xcode images-validate

build:
	$(GO) build -ldflags "$(LDFLAGS)" -o bin/grove ./cmd/grove

test:
	$(GO) vet ./...
	$(GO) test ./...

ui:
	cd ui && npm ci && npm run build

# Builds the lean macOS and Linux Tart worker images locally. Mac-only — see
# docs/IMAGES.md. Does not push; run `tart push` yourself once you've eyeballed the result.
images: images-macos
	cd images/linux-worker && packer init . && packer validate . && packer build .

images-macos: images-nomad
	cd images/macos-worker && packer init . && packer validate . && packer build .

# Build the pinned native Nomad dependency outside the VM. Reuses a verified artifact.
.PHONY: images-nomad
images-nomad:
	./scripts/build-worker-nomad.sh

# Full Xcode is opt-in and produces a distinct local VM.
images-macos-xcode: images-nomad
	cd images/macos-worker && packer init . && packer validate -var-file=xcode.pkrvars.hcl . && packer build -var-file=xcode.pkrvars.hcl .

# Lean Tahoe guest for sharing a compatible host Xcode application at runtime.
images-macos-host-xcode: images-nomad
	cd images/macos-worker && packer init . && packer validate -var-file=host-xcode.pkrvars.hcl . && packer build -var-file=host-xcode.pkrvars.hcl .

# Validate all macOS profiles without downloading or starting a VM.
images-validate: images-nomad
	packer fmt -check -recursive images/macos-worker
	cd images/macos-worker && packer init . && packer validate . && packer validate -var-file=xcode.pkrvars.hcl . && packer validate -var-file=host-xcode.pkrvars.hcl .

clean:
	rm -rf bin dist ui/dist internal/server/dist
