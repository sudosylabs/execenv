SHELL := /bin/sh

.PHONY: check test test-race vet build bake

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

# Operator/CI bake. Not part of check and not invoked by Ensure.
# KERNEL is a local vmlinux. SOURCE defaults to the universal-class pin.
# Dockerfile builds an extra catalog id instead of SOURCE.
OUT ?= out
KERNEL ?=
SOURCE ?=
DOCKERFILE ?=
ID ?= default
SIZE ?=
AGENT ?=
bake:
	@test -n "$(KERNEL)" || (echo "KERNEL=path/to/vmlinux is required" >&2; exit 2)
	go run ./cmd/execenv bake -out $(OUT) -kernel $(KERNEL) -id $(ID) \
		$(if $(SOURCE),-source $(SOURCE),) \
		$(if $(DOCKERFILE),-dockerfile $(DOCKERFILE),) \
		$(if $(SIZE),-size $(SIZE),) \
		$(if $(AGENT),-agent $(AGENT),)
