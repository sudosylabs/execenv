# execenv

`github.com/sudosylabs/execenv` occupies **isolated execution grants on one host**.

A caller checks readiness, ensures a grant, projects a POSIX tree, attaches one PTY, freezes and thaws I/O, and revokes. Isolation stays behind that contract. This repository is both the Go module you import and the installable host daemon that serves the same interface.

License: [Apache-2.0](LICENSE).

## Contents

- [What you get](#what-you-get)
- [Two machines, two installs](#two-machines-two-installs)
- [Pin a version](#pin-a-version)
- [Local development (no isolation hardware)](#local-development-no-isolation-hardware)
- [Operator: turn a Linux machine into a Host](#operator-turn-a-linux-machine-into-a-host)
- [Caller: talk to that Host from Go](#caller-talk-to-that-host-from-go)
- [Grant lifetime](#grant-lifetime)
- [Tree projection](#tree-projection)
- [Terminal](#terminal)
- [Network](#network)
- [Errors](#errors)
- [Catalog Images](#catalog-images)
- [Upgrade](#upgrade)
- [This repository](#this-repository)
- [Limits](#limits)

## What you get

| Piece | Who uses it | What it is |
| --- | --- | --- |
| Go module | Application developer | `Host` / `Env` / `Terminal` types |
| `execenv` | The Host process | Daemon (and the guest Agent baked into disks) |
| `execenvctl` | Operator | Bootstrap, install Images, status, upgrade |
| Catalog Image | Operator installs; caller names an id | Already-on-host kernel + root filesystem |

Vocabulary:

- **Host** — one machine that can occupy grants. One client value talks to exactly one Host.
- **Grant** — caller-chosen occupancy of at most one isolated environment on that Host.
- **Environment** — the isolated, non-authoritative projection attached to one grant, with at most one terminal. The guest home is `/workspace`.
- **Image** — a named disk already on the Host. It is not pulled when a grant starts.
- **Manager** — `execenvctl`. Grant callers do not import it.
- **Agent** — guest helper that owns the home directory and the login shell.

The tree you project is **not** durable authority. You keep the real files. Losing a grant must not be treated as losing caller state you already have.

## Two machines, two installs

They never mix.

| Machine | Person | Installs | Needs Go? |
| --- | --- | --- | --- |
| Your application | Developer | the Go module in `go.mod` | yes |
| Linux isolation Host | Operator | binaries (`execenvctl`, then `execenv`) and catalog disks | **no** |

The operator never `go get`s anything. The developer never bakes a disk on the Host. A git tag such as `v1.2.3` is the join key: the module version, the binary stamp, and the GitHub Release the Manager fetches.

End users never see the Host address. Placement across several Hosts is the caller’s job. execenv talks to exactly one machine.

## Pin a version

On the application:

```sh
go get github.com/sudosylabs/execenv@v1.2.3
```

On the Host, download **that same tag’s** operator binary (linux/amd64). Do not follow a moving `latest` pointer.

```sh
curl -fsSL -o execenvctl \
  https://github.com/sudosylabs/execenv/releases/download/v1.2.3/execenvctl
chmod +x execenvctl
sudo mv execenvctl /usr/local/bin/
execenvctl --version
# version=1.2.3
# build=…
# tag=v1.2.3
```

`remote.New` and the daemon compare stamps at auth. A `v1.2.3` client cannot occupy a `v1.4.0` Host (`ErrUnavailable`). You do not write that check in application code.

Unstamped (`dev`) `execenvctl` refuses GitHub fetches unless you pass `--release-url` or `EXECENV_RELEASE_URL` to a same-layout mirror.

## Local development (no isolation hardware)

The in-memory adapter is for tests and laptops. It is **not** isolated. Production TLS refuses it.

```go
package main

import (
	"context"
	"fmt"
	"io"
	"log"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/memory"
)

func main() {
	ctx := context.Background()
	host, err := memory.New(memory.Config{
		Images: []execenv.Image{"default", "python"},
		Slots:  2,
	})
	if err != nil {
		log.Fatal(err)
	}

	rep, err := host.Ready(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("usable=%v images=%v slots=%d\n", rep.Usable, rep.Images, rep.Slots)

	env, err := host.Ensure(ctx, execenv.Spec{
		ID:    "grant-1",
		Image: "default",
	})
	if err != nil {
		log.Fatal(err)
	}
	defer host.Revoke(ctx, env.ID())

	err = env.ReplaceTree(ctx, execenv.Tree{
		{
			Path:    "hello.py",
			Kind:    execenv.KindFile,
			Version: "v1",
			Data:    []byte("print('hello')\n"),
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	term, err := env.Attach(ctx, execenv.Window{Cols: 80, Rows: 24})
	if err != nil {
		log.Fatal(err)
	}
	defer term.Close()

	if _, err := term.Write([]byte("echo ready\n")); err != nil {
		log.Fatal(err)
	}
	buf := make([]byte, 64)
	n, err := term.Read(buf)
	if err != nil && err != io.EOF {
		log.Fatal(err)
	}
	fmt.Printf("pty: %q\n", buf[:n])
}
```

You can also run the daemon locally with the memory adapter (cleartext, loopback only):

```json
{
  "listen": "127.0.0.1:8443",
  "token": "dev-token",
  "security": "insecure_local",
  "adapter": "memory",
  "images": [{"id": "default"}, {"id": "python"}],
  "slots": 4,
  "network": "none",
  "grace": "30s"
}
```

```sh
go run ./cmd/execenv -config ./dev.json
```

`security: tls` refuses `adapter: memory`.

## Operator: turn a Linux machine into a Host

Needs: Linux, a usable isolation device, and root (or equivalent) for the system layout. Missing isolation hardware **fails closed**. There is no container fallback. Docker is not used on this machine.

Defaults:

| Flag | Default |
| --- | --- |
| `--prefix` | `/usr/local` |
| `--sysconf` | `/etc/execenv` |
| `--state` | `/var/lib/execenv` |
| `--listen` | `0.0.0.0:8443` |
| `--slots` | `8` |

### 1. Bootstrap (once)

```sh
sudo execenvctl bootstrap
```

That:

1. Checks the isolation device (fails with a clear error if it is missing).
2. Installs runtime and supervisor binaries if needed.
3. Downloads the `execenv` daemon from **this `execenvctl`’s tag**.
4. Writes Host configuration (listen address, TLS files, token, empty catalog, slots) under `--sysconf`.
5. Installs `execenv.service` and starts it.

The Host still cannot occupy grants: the catalog is empty. Copy the token (and CA) into the application that will dial. The Host does not register itself with your app.

```sh
sudo execenvctl status
# device=ok
# unit=active
# installed=
# release=v1.2.3
```

Useful flags: `--execenv ./execenv` (air-gap), `--no-start`, `--no-fetch`, `--insecure` (loopback labs only), `--listen`, `--slots`.

Re-running bootstrap is idempotent. It does not rotate the token. It does not fetch catalog disks.

### 2. Install Images

```sh
sudo execenvctl install python
sudo execenvctl list
```

`install` fetches that id from `…/releases/download/<tag>/`, verifies the kernel-then-rootfs hash, writes paths into Host configuration, and reloads the daemon. **`Ensure` never fetches.** If `python` is not installed, Ensure returns `ErrUnknownImage`.

Published ids (when the matching Release exists): `python`, `node`, `go`, `java`, `default` (large, often attached later), `fixture` (tiny, for live isolation tests).

```sh
sudo execenvctl remove python   # leaves the token alone
```

### 3. Point the application at the Host

Give the application:

- listen address (for example `host.example:8443`)
- the token from Host configuration (never log it)
- the TLS CA (production)

## Caller: talk to that Host from Go

Import the occupancy module and remote only. Do not import `internal/ctl`, `daemon`, or `isolated`.

```go
package runner

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"os"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/remote"
)

func dial(ctx context.Context) (execenv.Host, error) {
	pem, err := os.ReadFile("/etc/runner/execenv-ca.pem")
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("ca")
	}
	return remote.New(remote.Config{
		Address:  os.Getenv("EXECENV_ADDR"), // "192.0.2.10:8443"
		Security: remote.SecurityTLS,
		Token:    []byte(os.Getenv("EXECENV_TOKEN")),
		TLS: &tls.Config{
			RootCAs:    pool,
			ServerName: "execenv", // must match the Host certificate
		},
	})
}
```

`remote.New` opens one TLS connection and authenticates. From here the value is an `execenv.Host`. Keep it for the process lifetime (one client per Host).

Loopback tests may use `remote.SecurityInsecureLocal` on `127.0.0.1` only. Non-loopback insecure dials fail.

## Grant lifetime

A typical occupancy, in order:

### Ready

```go
rep, err := host.Ready(ctx)
if err != nil {
	return err
}
if !rep.Usable {
	return execenv.ErrUnavailable
}
// rep.Images is the ids on disk (for example "python").
// rep.Slots is remaining concurrent grants.
// rep.Release is the process stamp (display). Mismatch is already refused at dial.
```

Use this to **place**: skip a dead Host, skip a Host without the Image you need, skip a Host at capacity. execenv does not pick a user.

### Ensure

```go
env, err := host.Ensure(ctx, execenv.Spec{
	ID:      "job-7f3a", // you allocate and store this; 1–64 [A-Za-z0-9._-:]
	Image:   "python",
	Network: execenv.NetworkNone,
})
```

- Same id + same image + same network → the existing occupancy.
- Same id + different image or network → `ErrConflict`.
- Unknown catalog id → `ErrUnknownImage`.
- Host full → `ErrCapacity`.
- Allowlist requested but the Host has no dests → `ErrNetwork`.

Ensure does not start a fetch. The microVM may start lazily on first Attach.

### ReplaceTree then Watch, then Attach

Push your files, start harvesting guest writes, then attach the shell. See the sections below.

### Freeze / Thaw

```go
_ = env.Freeze(ctx) // PTY and tree I/O fail with ErrFrozen; the grant remains
_ = env.Thaw(ctx)
_ = env.ReplaceTree(ctx, lastAcknowledgedSnapshot)
term, err := env.Attach(ctx, execenv.Window{})
```

### Revoke

```go
_ = host.Revoke(ctx, "job-7f3a") // idempotent; destroys the occupancy
```

Files that mattered should already be in your store. Revoke is not a backup.

## Tree projection

`ReplaceTree` makes `/workspace` match the snapshot exactly: listed paths exist, unlisted paths are deleted.

```go
err := env.ReplaceTree(ctx, execenv.Tree{
	{Path: "src", Kind: execenv.KindDirectory},
	{
		Path:    "src/main.go",
		Kind:    execenv.KindFile,
		Version: "v12",
		Data:    []byte("package main\n"),
	},
	{
		Path:    "README.md",
		Kind:    execenv.KindFile,
		Version: "v3",
		Data:    nil, // "you already have this path at v3; do not transfer"
	},
})
```

- `Data: nil` on a file means skip the body; the Host must already have that Path at `Version`, or the call fails and the tree is unchanged.
- An empty non-nil slice is an empty file.
- Directories ignore `Data` and require an empty `Version`.

`Apply` is an atomic batch of create / replace / move / delete. `Expected` is an optimistic fence for files; mismatch is `ErrConflict` and nothing in the batch is applied.

Guest writes are **not** in your store until you harvest them:

```go
obs, err := env.Watch(ctx)
if err != nil {
	return err
}
defer obs.Close()

for {
	ev, err := obs.Next(ctx)
	if err != nil {
		if errors.Is(err, execenv.ErrLagged) {
			// stream dropped events; ReplaceTree from your authority, Watch again
		}
		return err
	}
	if ev.Op == execenv.OpCreate && ev.Path == "out.txt" {
		body, err := env.Open(ctx, ev.Path)
		if err != nil {
			return err
		}
		data, err := io.ReadAll(body)
		_ = body.Close()
		// persist data in *your* store, then ack
		_ = data
	}
}
```

`Watch` reports what the guest wrote, not your own ReplaceTree/Apply. After a non-nil `Next` error the observation is spent.

Paths are POSIX-relative, no leading `/`, no `..` escape, bounded (see [Limits](#limits)). Product-specific reserved roots are your problem.

## Terminal

One PTY per environment. A second Attach is `ErrBusy` until Close.

```go
term, err := env.Attach(ctx, execenv.Window{Cols: 120, Rows: 40})
if err != nil {
	return err
}
defer term.Close()

go io.Copy(term, fromUser) // bytes in
go io.Copy(toUser, term)   // bytes out

_ = term.Resize(ctx, execenv.Window{Cols: 80, Rows: 24})
```

Zero Cols or Rows means 80×24. PTY octets are not workspace mutations. Close is hangup (`ErrClosed`); it does **not** revoke the grant.

## Network

The grant caller cannot add destinations.

| Spec | Meaning |
| --- | --- |
| `NetworkNone` (default) | no NIC |
| `NetworkAllowlist` | the Host’s IPv4 dests only |

Destinations are operator-configured on the Host. If the grant asks for allowlist and the Host offers none, Ensure returns `ErrNetwork`. Changing network on a live grant is `ErrConflict`.

## Errors

Match with `errors.Is`. Wrapping preserves the sentinel (`execenv.OpError`).

| Sentinel | Meaning |
| --- | --- |
| `ErrInvalid` | bad id, image, path, or config |
| `ErrUnknownImage` | catalog id not on this Host |
| `ErrCapacity` | no remaining slots |
| `ErrConflict` | live grant, different image/network, or Apply fence |
| `ErrUnavailable` | Host unusable, or Release stamp mismatch |
| `ErrFrozen` | I/O while frozen |
| `ErrRevoked` | grant is gone |
| `ErrBusy` | a terminal is already attached |
| `ErrClosed` | terminal or observation closed (hangup, not revoke) |
| `ErrNotFound` | path missing in the projection |
| `ErrLagged` | Watch overflow; resync |
| `ErrTooLarge` | file or tree over the documented caps |
| `ErrConnection` | remote session lost |
| `ErrNetwork` | allowlist requested, Host offers none |

Tokens, tree bodies, and PTY octets must not enter ordinary logs.

## Catalog Images

An Image is a kernel plus a root filesystem with the toolchain already baked in, including the guest Agent. CI produces it (`scripts/bake`). Operators install an id. The daemon never pulls at grant time.

Language ids with Dockerfiles: `python`, `node`, `go`, `java`. `default` is a large universal-class disk, often published later onto the same tag. `fixture` is tiny and used for live isolation tests.

Pushing `v1.2.3` publishes stamped linux/amd64 binaries and the language disks. The large default disk is a separate, later attach that does not rewrite other hashes.

Bake is **CI-only**. It is not an `execenv` command and is not installed on the grant Host. The Host does not need Docker.

```sh
# CI runner, not the grant Host
scripts/bake --out ./out --kernel ./vmlinux --agent ./execenv \
  --id python --dockerfile ./catalog/python/Dockerfile
```

## Upgrade

On the Host, after you install a newer `execenvctl` for the target tag:

```sh
sudo execenvctl upgrade
```

That replaces `execenv` and `execenvctl` from this binary’s tag, then reinstalls every catalog id already on the Host so the baked Agent matches. Token, TLS files, listen address, slots, and allowlist are left alone. If an installed id is missing from the new index, upgrade fails **before** replacing binaries.

If binaries and disks already match: `upgraded=already current`.

## This repository

```sh
make check          # test, race, vet
make build          # stamps Release/Build/Tag from git when on an exact tag
execenv -config PATH
execenv -version
execenvctl --version
```

Pull requests run `make check`. Catalog-related pull requests smoke-bake `fixture` with a dummy kernel. A version tag publishes binaries and language disks. Isolation and network guest proofs are opt-in (`make isolation`, `make certify`) and need hardware.

Grant callers import:

```go
github.com/sudosylabs/execenv
github.com/sudosylabs/execenv/remote
```

Tests may also import `github.com/sudosylabs/execenv/memory` and `github.com/sudosylabs/execenv/execenvtest`.

## Limits

| Cap | Value |
| --- | --- |
| Grant id / Image id | 1–64 characters `[A-Za-z0-9._-:]` |
| Tree entries per ReplaceTree or Apply | 500 |
| One file body | 10 MiB |
| Sum of file bodies in one call | 50 MiB |
| Path UTF-8 length | 1024 |
| Path segments | 16 |
| One segment | 255 bytes |
| Concurrent terminals per environment | 1 |
| Hosts per client value | 1 |
