SHELL := /bin/sh

.PHONY: check test test-race vet build bake

check: test test-race vet

build:
	go build -o execenv ./cmd/execenv
	go build -o execenvctl ./cmd/execenvctl

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

# Live isolation tests need a Linux host with the isolation device and
# supervisor binaries. They are not part of check. Network allowlist
# guest-side proofs are in isolated/network_linux_test.go (root) and
# TestLiveAllowlistDeniesFromGuest (isolation hardware).
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

# A Linux isolation host is installed with execenvctl bootstrap, then
# execenvctl install <id>. There is no make target and no shell installer.
