# Contributing to execenv

Thank you for helping improve execenv. Contributions may include bug reports,
feature proposals, documentation, tests, and code.

By participating, you agree to follow the project
[Code of Conduct](CODE_OF_CONDUCT.md).

## Before You Start

- Search existing issues and pull requests before opening a duplicate.
- For a substantial feature or architecture change, open an issue first so the
  approach can be discussed.
- Keep each change focused. Avoid unrelated refactors, dependency upgrades, or
  repository-wide formatting.
- Report suspected vulnerabilities through the private process in
  [`SECURITY.md`](SECURITY.md), never in a public issue with technical details.
- Never include host tokens, TLS private keys, tree bodies, PTY octets, catalog
  hashes, or other secrets in an issue, log, test fixture, commit, or pull
  request.

## Development Setup

Clone the repository and create a branch from an up-to-date `main`:

```bash
git clone https://github.com/sudosylabs/execenv.git
cd execenv
git switch main
git pull --ff-only
git switch -c feat/short-description
```

Use a branch name that describes the outcome, such as `feat/watch-lag`,
`fix/tls-listen`, or `docs/host-bootstrap`.

You need:

- Go 1.25 or newer (see `go.mod`)
- GNU Make
- A POSIX shell

Isolation hardware is not required for `make check`. Live isolation and
network proofs are opt-in and need a Linux isolation host.

## Command Interface

```bash
make check          # test, race, and vet
make build          # execenv and execenvctl, stamped when HEAD is a git tag
make isolation      # opt-in; needs isolation hardware
make certify        # opt-in; needs fixture disks
```

`make check` is the hermetic gate. Pull requests run it in CI. Do not add
bake, Docker, or isolation hardware to that gate.

## Repository Structure

| Path | Purpose |
|------|---------|
| `.` | Occupancy contract: `Host`, `Env`, `Terminal`, tree types, errors |
| `remote/` | Shared wire: `remote.New` and `remote.Serve` |
| `memory/` | In-process adapter for tests and local development |
| `isolated/` | Production isolation adapter |
| `daemon/` | Host process: load configuration and serve the remote contract |
| `agent/` | Guest helper for home directory and login shell |
| `cmd/execenv/` | Daemon and guest agent binary |
| `cmd/execenvctl/` | Operator manager |
| `catalog/` | Image recipes published by CI bake |
| `scripts/` | CI bake, assemble, and pin-image; not installed on the grant host |
| `execenvtest/` | Conformance suite for adapters |

Grant callers import `github.com/sudosylabs/execenv` and
`github.com/sudosylabs/execenv/remote`. They do not import the manager, the
daemon, or the isolation adapter.

## Engineering Expectations

- The public occupancy API does not name a particular hypervisor.
- One `Host` value is one machine. Placement across hosts is the caller's job.
- Production isolation fails closed without a usable isolation device. The
  in-memory adapter is for tests and local development only. TLS refuses it.
- Trees are projections. Callers own durable file authority. This module never
  accepts storage credentials from a caller filesystem.
- PTY octets, tree bodies, and grant tokens never enter ordinary logs.
- `remote.New` and `remote.Serve` share one wire so the protocol cannot drift.
  The daemon serves this module; it does not invent a second API.
- `Ensure` never fetches images. A missing catalog id is `ErrUnknownImage`.
- Match the style of nearby code. Comments should explain non-obvious
  invariants, not restate the code.

Sensitive paths (CI, catalog, bake, isolation, module pin, this
`CODEOWNERS` file) require review from the listed code owner.

## Testing

From the repository root:

```bash
make check
```

That is `go test ./...`, `go test -race ./...`, and `go vet ./...`.

Bug fixes need a regression test at the lowest layer that demonstrates the
failure. Adapter changes should keep the conformance suite green
(`execenvtest`).

If a required check cannot run on your machine, explain why in the pull
request and list the checks you did run. Isolation-tagged tests and guest
network proofs are allowed to stay opt-in.

## Commits

Use concise messages that describe the outcome, for example:

```text
Refuse a remote dial when Release stamps differ.
```

Create signed commits when your repository configuration requires signing. Do
not bypass a signing requirement with an unsigned commit.

## Pull Requests

Open pull requests against `main`. A good pull request should:

1. Explain what changed and why.
2. Stay limited to one coherent outcome.
3. Link the relevant issue, when one exists.
4. Include regression coverage for bug fixes and behavior changes.
5. Say which commands you ran (`make check` at minimum).
6. Avoid generated files, unrelated formatting, and dependency changes unless
   they are required by the contribution.

Review feedback is part of the collaboration process. Keep follow-up commits
focused, and resolve review threads only after the concern has been addressed.

## Licensing

execenv is distributed under the [Apache License 2.0](LICENSE). Unless
explicitly stated otherwise, contributions accepted into this repository are
distributed under the same license.
