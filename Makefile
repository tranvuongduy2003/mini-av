GO ?= go
GOFMT ?= gofmt
BINARY := miniav$(shell $(GO) env GOEXE)

.PHONY: all build run fmt vet test test-race check

all: build

build:
	$(GO) build -o $(BINARY) ./cmd/miniav

run:
	$(GO) run ./cmd/miniav $(ARGS)

fmt:
	$(GOFMT) -w .

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

check: fmt vet test test-race
