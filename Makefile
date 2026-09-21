GO ?= go
GOFMT ?= gofmt
PROTOC ?= protoc
BINARY := miniav$(shell $(GO) env GOEXE)
SCANNER_A := bin/scanner-a$(shell $(GO) env GOEXE)
SCANNER_B := bin/scanner-b$(shell $(GO) env GOEXE)
SCANNER_C := bin/scanner-c$(shell $(GO) env GOEXE)

.PHONY: all build generate run fmt vet test test-race check

all: build

build:
	mkdir -p bin
	$(GO) build -o $(BINARY) ./cmd/miniav
	$(GO) build -o $(SCANNER_A) ./cmd/scanner-a
	$(GO) build -o $(SCANNER_B) ./cmd/scanner-b
	$(GO) build -o $(SCANNER_C) ./cmd/scanner-c

generate:
	$(PROTOC) --go_out=. --go_opt=module=miniav --go-grpc_out=. --go-grpc_opt=module=miniav api/scanner/v1/scanner.proto

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
