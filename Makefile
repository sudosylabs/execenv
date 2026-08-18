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

# Live isolation tests need a Linux host with the isolation device and
# supervisor binaries. They are not part of check.
isolation:
	EXECENV_ISOLATION=1 go test -tags=isolation -count=1 ./isolated
