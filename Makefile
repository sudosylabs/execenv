SHELL := /bin/sh

.PHONY: check test test-race vet build bake bootstrap

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

# CI bake. Not part of check, not an execenv command, not used on the
# grant host. KERNEL is a local vmlinux. AGENT is a linux execenv binary.
# SOURCE defaults to the universal-class pin.
OUT ?= out
KERNEL ?=
SOURCE ?=
DOCKERFILE ?=
ID ?= default
SIZE ?=
AGENT ?=
bake:
	@test -n "$(KERNEL)" || (echo "KERNEL=path/to/vmlinux is required" >&2; exit 2)
	@test -n "$(AGENT)" || (echo "AGENT=path/to/linux-execenv is required" >&2; exit 2)
	scripts/bake --out $(OUT) --kernel $(KERNEL) --agent $(AGENT) --id $(ID) \
		$(if $(SOURCE),--source $(SOURCE),) \
		$(if $(DOCKERFILE),--dockerfile $(DOCKERFILE),) \
		$(if $(SIZE),--size $(SIZE),)

# Operator install on a Linux isolation host. Not an execenv command.
# ARTIFACTS is a directory with vmlinux, rootfs.ext4, catalog.json.
# EXECENV is a linux execenv binary. Missing isolation device fails closed.
ARTIFACTS ?=
EXECENV ?=
RELEASE ?=
bootstrap:
	@if [ -n "$(RELEASE)" ]; then \
		scripts/bootstrap --release $(RELEASE) $(if $(EXECENV),--execenv $(EXECENV),); \
	else \
		test -n "$(ARTIFACTS)" || (echo "ARTIFACTS=dir or RELEASE=url is required" >&2; exit 2); \
		test -n "$(EXECENV)" || (echo "EXECENV=path/to/linux-execenv is required" >&2; exit 2); \
		scripts/bootstrap --artifact-dir $(ARTIFACTS) --execenv $(EXECENV); \
	fi
