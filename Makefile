.PHONY: build test vet fmt check clean dist

VERSION ?= $(shell git describe --tags --match 'v*' --always --dirty 2>/dev/null | sed 's/^v//')
ifeq ($(strip $(VERSION)),)
VERSION := 0.1.0-dev
endif

LDFLAGS ?= -X lax/internal/cli.version=$(VERSION)

build:
	go build -ldflags "$(LDFLAGS)" -o lax ./cmd/lax

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

check: vet test build

dist:
	VERSION='$(VERSION)' ./scripts/release.sh

clean:
	rm -f lax
