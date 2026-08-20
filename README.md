<p align="center">
  <img src="public/image.png" alt="execenv" width="128" />
</p>

<h1 align="center">execenv</h1>

<p align="center">
  <strong>Occupy isolated execution grants on one host.</strong>
</p>

<p align="center">
  Check readiness, ensure a grant, project a workspace tree, attach one
  PTY, freeze and thaw I/O, and revoke. Isolation stays behind that
  contract.
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go&logoColor=white" alt="Go 1.25+" />
  <img src="https://img.shields.io/badge/linux%2Famd64-host-1793D1?logo=linux&logoColor=white" alt="linux/amd64 host" />
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache%202.0-6D28D9" alt="Apache 2.0 license" /></a>
</p>

<p align="center">
  <a href="https://github.com/sudosylabs/execenv/actions/workflows/check.yml"><img src="https://github.com/sudosylabs/execenv/actions/workflows/check.yml/badge.svg" alt="check" /></a>
  <a href="https://github.com/sudosylabs/execenv/actions/workflows/release.yml"><img src="https://github.com/sudosylabs/execenv/actions/workflows/release.yml/badge.svg" alt="release" /></a>
  <a href="https://pkg.go.dev/github.com/sudosylabs/execenv"><img src="https://pkg.go.dev/badge/github.com/sudosylabs/execenv.svg" alt="Go reference" /></a>
</p>

This repository is two things that share one interface:

1. A Go module your application imports (`Host`, `Env`, `Terminal`).
2. An installable Linux host process that serves that same interface.

Your application is the authority for files. The grant is a projection: if
it disappears, you still have whatever you already stored.

---

## Build a client

Install the module into the project that will start grants, push files,
and attach a shell.

```sh
go get github.com/sudosylabs/execenv@v1.2.3
```

Pin the same tag the host binaries were built from. A `v1.2.3` client
refuses to occupy a host stamped `v1.4.0`.

Import the occupancy API and, in production, the remote dialer:

```go
import (
	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/remote"
)
```

Do not import the operator tool, the daemon package, or the isolation
adapter. Those are for the host process, not for grant callers.

### Start against an in-process host

The in-memory adapter implements the same `Host` as production. Use it in
unit tests and on a laptop. It is not isolated; production TLS refuses it.

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/memory"
)

func main() {
	ctx := context.Background()
	host, err := memory.New(memory.Config{
		Images: []execenv.Image{"python", "default"},
		Slots:  4,
	})
	if err != nil {
		log.Fatal(err)
	}
	run(ctx, host)
}

func run(ctx context.Context, host execenv.Host) {
	rep, err := host.Ready(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("usable=%v images=%v slots=%d release=%s\n",
		rep.Usable, rep.Images, rep.Slots, rep.Release)
}
```

Every method below takes that `host`. Swap `memory.New` for `remote.New`
when you have a real host; the rest of the code does not change.

### Connect to a running host

```go
package runner

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/remote"
)

func Dial() (execenv.Host, error) {
	pem, err := os.ReadFile(os.Getenv("EXECENV_CA"))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("ca pem")
	}
	return remote.New(remote.Config{
		Address:  os.Getenv("EXECENV_ADDR"), // "host.example:8443"
		Security: remote.SecurityTLS,
		Token:    []byte(os.Getenv("EXECENV_TOKEN")),
		TLS: &tls.Config{
			RootCAs:    pool,
			ServerName: os.Getenv("EXECENV_SERVER_NAME"),
		},
	})
}
```

`remote.New` dials, authenticates with the token, and checks the process
stamp. Keep one client per host for the life of the process.

Cleartext is loopback-only:

```go
host, err := remote.New(remote.Config{
	Address:  "127.0.0.1:8443",
	Security: remote.SecurityInsecureLocal,
	Token:    []byte("dev-token"),
})
```

A non-loopback insecure address is rejected.

`execenv.RequireIsolated(host)` fails closed when the adapter is not
isolated. Use it in production composition so the in-memory host cannot
be selected by accident.

### Ready

```go
rep, err := host.Ready(ctx)
if err != nil {
	return err
}
if !rep.Usable {
	return execenv.ErrUnavailable
}
```

`Images` is the catalog ids on that machine. `Slots` is how many more
grants it will take. `Release` is the process stamp (display only; a
mismatch already failed at dial). One client talks to one host. If you
have several machines, you choose among them.

### Ensure

You allocate the grant id and store it. It is not derived by execenv.

```go
env, err := host.Ensure(ctx, execenv.Spec{
	ID:      "job-7f3a",
	Image:   "python",
	Network: execenv.NetworkNone,
})
if err != nil {
	return err
}
```

| Situation | Result |
| --- | --- |
| New id | occupies a grant |
| Same id, same image, same network | returns the existing occupancy |
| Same id, different image or network | `ErrConflict` |
| Image not installed on the host | `ErrUnknownImage` |
| No remaining slots | `ErrCapacity` |
| Allowlist requested, host has no dests | `ErrNetwork` |

Ids and image names are 1–64 characters in `[A-Za-z0-9._-:]`. Ensure does
not download images. The guest may start on first `Attach`, not at Ensure.

### ReplaceTree

Makes `/workspace` match the snapshot: listed paths exist, everything
else is deleted.

```go
err := env.ReplaceTree(ctx, execenv.Tree{
	{Path: "src", Kind: execenv.KindDirectory},
	{
		Path:    "src/main.py",
		Kind:    execenv.KindFile,
		Version: "v1",
		Data:    []byte("print('hello')\n"),
	},
	{
		Path:    "data.json",
		Kind:    execenv.KindFile,
		Version: "v4",
		Data:    nil, // already on the host at v4; do not send the body
	},
})
```

`Version` is an opaque token you choose. The host compares it for
equality and never parses it.

- `Data: nil` on a file means skip the transfer. The host must already
  have that path at that version, or the call fails and the tree is
  unchanged.
- `Data: []byte{}` is an empty file.
- Directories ignore `Data` and require an empty `Version`.

After reconnect or thaw, replace from **your** last acknowledged
snapshot, not from unacked guest bytes.

### Apply

Atomic incremental edits. The whole batch succeeds or nothing changes.

```go
err := env.Apply(ctx, execenv.Batch{Mutations: []execenv.Mutation{
	{
		Op:       execenv.OpReplace,
		Path:     "src/main.py",
		Kind:     execenv.KindFile,
		Version:  "v2",
		Expected: "v1", // fence: fail with ErrConflict if the file is not v1
		Data:     []byte("print('hello world')\n"),
	},
	{
		Op:   execenv.OpCreate,
		Path: "out",
		Kind: execenv.KindDirectory,
	},
}})
```

Empty `Expected` means apply unconditionally. `OpMove` uses `From` (old
path) and `Path` (new path). `OpDelete` removes a path.

### Watch and Open

`Watch` is how the guest reports files **it** created, replaced, moved,
or deleted. Your own `ReplaceTree` / `Apply` calls do not appear here.

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
			// events were dropped; ReplaceTree from your store, Watch again
		}
		return err
	}
	switch ev.Op {
	case execenv.OpCreate, execenv.OpReplace:
		body, err := env.Open(ctx, ev.Path)
		if err != nil {
			return err
		}
		data, err := io.ReadAll(body)
		_ = body.Close()
		if err != nil {
			return err
		}
		saveInYourStore(ev.Path, data) // you own durable files
	case execenv.OpDelete:
		deleteFromYourStore(ev.Path)
	case execenv.OpMove:
		moveInYourStore(ev.From, ev.Path)
	}
}
```

After a non-nil `Next` error the observation is spent. Open a new Watch
once you have resynchronized.

Paths are POSIX-relative: no leading `/`, no `..`, no NUL, UTF-8.

### Attach a terminal

One PTY per environment. Bytes are not workspace mutations.

```go
term, err := env.Attach(ctx, execenv.Window{Cols: 120, Rows: 40})
if err != nil {
	return err
}
defer term.Close()

go copyTo(term, userInput)  // io.Reader from your UI
go copyTo(userOutput, term) // io.Writer to your UI

_ = term.Resize(ctx, execenv.Window{Cols: 80, Rows: 24})
```

Zero Cols or Rows means 80×24. A second `Attach` returns `ErrBusy` until
`Close`. Close is hangup (`ErrClosed`); the grant stays.

### Freeze and thaw

```go
if err := env.Freeze(ctx); err != nil {
	return err
}
// tree and PTY calls fail with ErrFrozen; the occupancy remains
if err := env.Thaw(ctx); err != nil {
	return err
}
if err := env.ReplaceTree(ctx, lastAcknowledged); err != nil {
	return err
}
term, err := env.Attach(ctx, execenv.Window{})
```

### Revoke

```go
err := host.Revoke(ctx, "job-7f3a") // also env.Revoke(ctx); both are idempotent
```

The guest is destroyed. Keep anything you still need in your own store
before this call.

### Network

The grant caller cannot add destinations.

```go
env, err := host.Ensure(ctx, execenv.Spec{
	ID:      "job-7f3a",
	Image:   "python",
	Network: execenv.NetworkAllowlist, // or NetworkNone
})
```

`NetworkNone` gives the grant no NIC. `NetworkAllowlist` gives it the
IPv4 destinations configured on the host. If the host has none, Ensure
returns `ErrNetwork`. Changing network on a live grant is `ErrConflict`.

### Errors

Match with `errors.Is`. Wrappers preserve the sentinel.

| Error | When |
| --- | --- |
| `ErrInvalid` | bad id, image, path, or config |
| `ErrUnknownImage` | that catalog id is not on the host |
| `ErrCapacity` | no remaining slots |
| `ErrConflict` | live grant mismatch, or Apply fence |
| `ErrUnavailable` | host unusable, or release stamp mismatch |
| `ErrFrozen` | I/O while frozen |
| `ErrRevoked` | grant is gone |
| `ErrBusy` | a terminal is already attached |
| `ErrClosed` | terminal or observation closed (not revoke) |
| `ErrNotFound` | path missing |
| `ErrLagged` | Watch overflow; resync |
| `ErrTooLarge` | file or tree over the cap |
| `ErrConnection` | remote session lost |
| `ErrNetwork` | allowlist requested, host offers none |

Do not log tokens, tree bodies, or PTY octets.

### Limits

| Cap | Value |
| --- | --- |
| Grant id / image id | 1–64 characters `[A-Za-z0-9._-:]` |
| Entries per ReplaceTree or Apply | 500 |
| One file body | 10 MiB |
| Sum of bodies in one call | 50 MiB |
| Path length | 1024 bytes UTF-8 |
| Path segments | 16 |
| One segment | 255 bytes |
| Terminals per environment | 1 |

The workspace inside the guest is `/workspace`.

---

## Create a host

This section is for the Linux machine that will occupy grants. It does
not use Go. You install binaries and catalog disks, then give the
application the listen address, token, and certificate.

### What the machine needs

- Linux on **amd64**
- A usable isolation device at `/dev/kvm` (readable and writable by the
  host process, typically the `kvm` group)
- Root, or equivalent, for the default layout
- Network reachability from the application on port **8443** (or the
  listen address you choose)

Missing isolation hardware fails closed. There is **no container
fallback**. Docker is not used on this machine and is not a substitute.

**Use a dedicated Linux machine, not a nested VM, when you can.** The
isolation device is itself a virtual machine monitor. Running that inside
another hypervisor (a cloud VM with nested virtualization, QEMU-in-QEMU,
WSL2, Docker Desktop’s Linux VM, many CI runners) often means:

- `/dev/kvm` is missing or not writable
- nested virt is advertised but disabled in the BIOS or by the provider
- extra latency, missing CPU features, or unstable guests

Bare metal, or a provider that exposes KVM to the guest as a first-class
device, is the production path. A “nested Linux” lab can work for
exploration if `/dev/kvm` opens, but it is the first thing to blame when
Ready is unusable or grants fail to start.

### Install the operator tool

Download `execenvctl` from the **same tag** your application pinned.

```sh
curl -fsSL -o execenvctl \
  https://github.com/sudosylabs/execenv/releases/download/v1.2.3/execenvctl
chmod +x execenvctl
sudo mv execenvctl /usr/local/bin/
execenvctl --version
```

You should see `version=1.2.3` and `tag=v1.2.3`. Do not use a moving
`latest` URL.

### Bootstrap

```sh
sudo execenvctl bootstrap
```

Defaults:

| Flag | Default |
| --- | --- |
| `--prefix` | `/usr/local` |
| `--sysconf` | `/etc/execenv` |
| `--state` | `/var/lib/execenv` |
| `--listen` | `0.0.0.0:8443` |
| `--slots` | `8` |
| `--device` | `/dev/kvm` |

What bootstrap does, in order:

1. Opens the isolation device. If that fails, it stops and writes
   nothing. The error says the device is missing or not usable, and that
   there is no container fallback.
2. Creates the bin, state, and config directories.
3. Installs the `execenv` daemon binary (from this tag, or `--execenv`
   for an air-gapped file).
4. Installs the runtime and supervisor pair, fetching them if they are
   not already on `PATH` (unless `--no-fetch`).
5. Writes Host configuration and TLS files (see below).
6. Writes `execenv.service` and enables it (unless `--no-start`).

The catalog is still empty. The host cannot occupy grants until you
install at least one Image.

```sh
sudo execenvctl status
# device=ok
# unit=active
# installed=
# release=v1.2.3
```

Useful flags: `--listen`, `--slots`, `--execenv ./execenv`, `--no-start`,
`--no-fetch`. `--insecure` writes cleartext for loopback labs only; do
not use it in production.

Re-running bootstrap is idempotent. It does not rotate the token. It
does not drop TLS files even if you pass `--insecure` the second time.
It does not fetch catalog disks.

### Certificates and the token

On first production bootstrap (no `--insecure`), execenvctl generates a
self-signed server certificate:

- ECDSA P-256 key
- Subject CN `execenv`
- SAN: DNS name `execenv`, IP `127.0.0.1`
- Not valid for more than about ten years
- Certificate `/etc/execenv/tls.crt` (mode `0644`)
- Private key `/etc/execenv/tls.key` (mode `0600`)

Host configuration records those paths and `security: tls`. The
application must trust `tls.crt` and set `TLS.ServerName` to `execenv`,
**or** you replace the pair with a certificate that matches the hostname
clients actually dial (a public CA, an internal CA, Let’s Encrypt, and
so on). Leave the same paths in Host configuration after you replace the
files, then restart the unit.

A 32-byte random token is written into Host configuration (mode `0600`).
It is never printed. Copy it once into the application’s secret store.
Ordinary logs must never contain it.

For a public hostname, a typical client config is: listen
`0.0.0.0:8443`, replace `tls.crt` / `tls.key` with a cert whose SAN is
that hostname, set the application’s `ServerName` to the same name, and
open 8443 on the firewall only to the application network.

### Install catalog images

Images are kernel plus root filesystem, with the toolchain and guest
agent already baked in. They are not Docker tags and are not pulled when
a grant starts.

```sh
sudo execenvctl install python
sudo execenvctl list
```

`install` fetches that id from this binary’s tagged release, verifies the
kernel-then-rootfs hash, writes local paths into Host configuration, and
reloads the daemon.

Ids you can install when the release includes them: `python`, `node`,
`go`, `java`. `default` is a large universal-class disk, often published
onto the same tag later. `fixture` is a tiny disk for live isolation
tests.

```sh
sudo execenvctl remove python   # does not touch the token
```

Until an id is installed, `Ensure` of that id is `ErrUnknownImage`.

### Give the application what it needs

1. Address (`host.example:8443`)
2. Token from Host configuration
3. CA / server certificate the client should trust
4. `ServerName` that matches the certificate

The host does not call home. Placement of users onto this machine is
entirely the application’s job.

### Upgrade

Install a newer `execenvctl` for the target tag, then:

```sh
sudo execenvctl upgrade
```

That replaces `execenv` and `execenvctl` from this binary’s tag, then
reinstalls every catalog id already on the host so the guest agent
matches. Token, TLS files, listen address, slots, and allowlist stay.
If an installed id is missing from the new index, upgrade fails before
binaries move. When everything already matches: `upgraded=already current`.

---

## This repository

```sh
make check    # test, race, vet
make build    # stamps Release, Build, and Tag when HEAD is an exact git tag
```

Pull requests run `make check`. Catalog-related pull requests smoke-bake
`fixture`. Pushing `v*` publishes linux/amd64 binaries and language
disks. Isolation hardware tests are opt-in (`make isolation`,
`make certify`).

Bake (`scripts/bake`) is CI-only. It is not an `execenv` command and is
not installed on the grant host.

```sh
scripts/bake --out ./out --kernel ./vmlinux --agent ./execenv \
  --id python --dockerfile ./catalog/python/Dockerfile
```

## Community

- [Contributing](CONTRIBUTING.md)
- [Security policy](SECURITY.md)
- [Code of Conduct](CODE_OF_CONDUCT.md)
