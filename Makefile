VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

GOFLAGS ?= -trimpath
ROOTFS  ?= ./rootfs
STATE   ?= /var/lib/sandboxd

.PHONY: build build-linux test test-race test-integration lint fmt vet run clean

build:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/sandboxd ./cmd/sandboxd
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/sandboxbench ./cmd/sandboxbench

# Cross-compile for a Linux host from any platform.
build-linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(or $(GOARCH),amd64) go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/sandboxd-linux-$(or $(GOARCH),amd64) ./cmd/sandboxd

test:
	go test ./...

test-race:
	go test -race ./...

# Linux-only integration tests: real namespaces, cgroups and seccomp. Needs
# root and cgroup v2 on a Linux host, or Docker anywhere (runs privileged).
test-integration:
	./scripts/test-linux.sh

lint: fmt vet
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run ./...; fi

fmt:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	go vet ./...
	GOOS=linux GOARCH=amd64 go vet ./...
	GOOS=linux GOARCH=arm64 go vet ./...

run: build
	sudo ./bin/sandboxd serve --rootfs $(ROOTFS) --state-dir $(STATE)

clean:
	rm -rf bin
