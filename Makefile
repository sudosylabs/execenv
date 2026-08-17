SHELL := /bin/sh

.PHONY: check test test-race vet build

check: test test-race vet

build:
	go build -o bin/execenv ./cmd/execenv

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...
