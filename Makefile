BINARY  := tassilo
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -ldflags "-X main.version=$(VERSION) -s -w"
GOFLAGS :=

.PHONY: build install clean fmt lint vet check

build:
	go build $(GOFLAGS) $(LDFLAGS) -o $(BINARY) .

install:
	go install $(GOFLAGS) $(LDFLAGS) .

clean:
	rm -f $(BINARY)

fmt:
	gofmt -w -s .

vet:
	go vet ./...

lint: fmt vet

check: lint
	go build $(GOFLAGS) $(LDFLAGS) -o $(BINARY) .

.DEFAULT_GOAL := build
