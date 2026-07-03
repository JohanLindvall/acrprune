BINARY  := acrprune
PKG     := ./cmd/acrprune
DISTDIR := dist

# Version is derived from git and injected into the binary. Override on the
# command line (e.g. `make build VERSION=v1.2.3`) to pin it.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# OS/ARCH pairs to cross-compile for `dist-all`.
PLATFORMS := linux/amd64 linux/arm64

# Pinned golangci-lint; bootstrapped into GOPATH/bin if not already present.
GOLANGCI_LINT_VERSION := v2.12.2
GOLANGCI_LINT         := $(shell go env GOPATH)/bin/golangci-lint

.PHONY: all build test vet lint clean dist-all

all: build

build:
	go build -ldflags '$(LDFLAGS)' -o $(BINARY) $(PKG)

test:
	go test ./...

vet:
	go vet ./...

lint: $(GOLANGCI_LINT)
	$(GOLANGCI_LINT) run

# Install the pinned golangci-lint if it is missing.
$(GOLANGCI_LINT):
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

# Cross-compile every entry in PLATFORMS into $(DISTDIR) as a .tar.gz, then
# write a sha256 checksum file covering all archives.
dist-all:
	@rm -rf $(DISTDIR)
	@mkdir -p $(DISTDIR)
	@for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; \
	  echo "building $$os/$$arch"; \
	  GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
	    go build -ldflags '$(LDFLAGS)' -o $(DISTDIR)/$(BINARY) $(PKG) || exit 1; \
	  tar -czf $(DISTDIR)/$(BINARY)-$(VERSION)-$$os-$$arch.tar.gz -C $(DISTDIR) $(BINARY); \
	  rm -f $(DISTDIR)/$(BINARY); \
	done
	cd $(DISTDIR) && sha256sum *.tar.gz > checksums-$(VERSION).txt

clean:
	rm -rf $(DISTDIR) $(BINARY)
