<p align="center">
  <img src="public/image.png" alt="execenv" width="128" />
</p>

<h1 align="center">execenv</h1>

<p align="center">
  <strong>Occupy isolated execution grants on one host.</strong>
</p>

<p align="center">
  Check readiness, ensure a grant, project a workspace journal, observe
  guest changes, run processes, control execution under a fence, and
  revoke. Isolation stays behind that contract.
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

1. A Go module your application imports (`Host`, `Env`).
2. An installable Linux host process that serves that same interface.

Your application is the authority for files. The grant is a projection: if
it disappears, you still have whatever you already stored.

---

## Build a client

Install the module into the project that will start grants, push files,
and run shells and commands.

```sh
go get github.com/sudosylabs/execenv@v1.2.3
```

Client and host need not share a release. The wire is the protobuf schema in
[`proto/execenv/v1`](proto/execenv/v1), spoken with Connect over HTTP/2, so
callers in other languages can generate a client from it. When it connects,
a client names the features it needs; a host that lacks one is refused at
once with `ErrFeature`, before any grant is occupied. The host separately
verifies the guest helper and its native journal before admitting an
environment.

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
		Images:   []execenv.Image{"python", "default"},
		Capacity: execenv.DefaultGrant.Times(4), // room for four default grants
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
	fmt.Printf("usable=%v images=%v networks=%v free memory=%d release=%s\n",
		rep.Usable, rep.Images, rep.Networks, rep.MemoryBytes.Free, rep.Release)
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

`remote.New` dials, sends the token as a bearer credential, and checks that
the host offers the features this client needs. A verified client
certificate may replace the token when the host enables mutual TLS. Keep one
client per host for the life of the process: it holds two HTTP/2
connections, one for control, processes and logs, one for file content, so
a large upload never delays a keystroke. `Timeout` bounds dialing and the
first exchange; `OperationTimeout` caps every call and defaults to 30
seconds.

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

`Images` is the catalog ids on that machine. `Networks` is the network modes
the host offers. `CPUMillis`, `MemoryBytes` and `DiskBytes` are its
capacity, each with `Total`, `Reserved` and `Free`: every grant reserves its
whole size from the moment it starts until it is revoked or lost. CPU totals
already include the operator's overcommit; memory is never overcommitted.
`Defaults` is what a grant gets when its Spec leaves a size unset, and
`GrantMax` the most one grant may ask for. `Grants` is how many grants the
host holds now, and `Lost` how many it has lost since it started (see Lost
grants). `Release` is the host's release, for display. One client talks to
one host. If you have several machines, you choose among them, by the
capacity each reports.

`Usable` false means the host takes no new grants; `Reason` says why.
`execenv.ReasonDraining` means its operator is draining it before a
being taken out of service: it keeps serving the grants it holds, and Ensure of a new id
returns `ErrDraining`. Place new grants elsewhere until it is usable again.

`Isolation` is the boundary class the host provides:

| Value | Boundary |
| --- | --- |
| `execenv.IsolationVM` (`"vm"`) | each grant is a hardware-virtualized microVM with its own guest kernel |
| `execenv.IsolationNone` (`"none"`) | in-memory adapter; tests only |

### Ensure

You allocate the grant id and store it. It is not derived by execenv.

```go
env, err := host.Ensure(ctx, execenv.Spec{
	ID:      "job-7f3a",
	Image:   "python",
	Network: execenv.NetworkNone,
	// Zero fields use the host's Defaults.
	Resources: execenv.Resources{CPUMillis: 2000, MemoryBytes: 2 << 30, DiskBytes: 8 << 30},
	// Revoke the grant two hours after it starts, and freeze it after
	// fifteen idle minutes.
	WallTimeout: 2 * time.Hour,
	IdleTimeout: 15 * time.Minute,
	IdleAction:  execenv.IdleFreeze,
	Labels:      map[string]string{"queue": "nightly"},
	Limits: execenv.Limits{
		DiskBytes: execenv.TokenBucket{Rate: 64 << 20, Burst: 256 << 20},
	},
})
var short *execenv.CapacityError
if errors.As(err, &short) {
	// short.Resource is what this host lacks; try another host.
}
if err != nil {
	return err
}
```

| Situation | Result |
| --- | --- |
| New id | occupies a grant |
| Same id and spec (Limits aside) | returns the existing occupancy |
| Same id, any other field different | `ErrConflict` |
| Image not installed on the host | `ErrUnknownImage` |
| Does not fit in what is free, or above `GrantMax` | `*CapacityError`, matching `ErrCapacity`, naming the short resource |
| Size below the minimums (100 CPU millis, 128 MiB, 64 MiB disk) | `ErrInvalid` |
| Host draining, id not held | `ErrDraining` |
| Id was lost and not yet revoked | `ErrLost` |
| Allowlist requested, host has no dests | `ErrNetwork` |
| Guest helper cannot serve the journal | `ErrUnavailable`; the guest is destroyed |
| Guest kernel lacks process events | `ErrUnavailable`; the guest is destroyed |

Ids and image names are 1–64 characters in `[A-Za-z0-9._-:]`. Concurrent
identical `Ensure` calls for one new id coalesce into one launch. Ensure does
not download images; occupying a new grant starts its guest, and it never
queues: a grant that does not fit is refused at once.

The guest sees enough virtual CPUs to use its `CPUMillis` and is throttled
to exactly that; `MemoryBytes` is its memory; `DiskBytes` is its workspace
disk, shared by the projected tree and the shell's scratch home.

A grant is busy while anything is attached to one of its processes and
after any call on it. Once idle for `IdleTimeout` the host applies
`IdleAction`: `IdleRevoke` revokes it; `IdleFreeze` freezes it as a fenced
freeze would, keeping the caller's fence revision, so `ControlStatus` shows
`frozen` and the caller thaws it with its next revision. `IdleSuspend`
freezes it the same way, then saves its running machine to the host's disk
and stops it: only its disk stays reserved, so its CPU and memory go to
other grants. The caller's thaw restores the machine where it stopped,
processes running and output resuming at the same offsets; while the host
has no room for it the thaw fails with `ErrCapacity` and the grant stays
suspended. The in-memory host freezes instead. `WallTimeout` revokes the grant that long
after it started, busy or not. Labels are the caller's, up to 16 pairs and
512 bytes; the host never reads them.

`Limits` throttle the grant's workspace disk (operations and bytes per
second) and its network device (bytes per second each way) with token
buckets: a rate, and a burst allowed once. A zero rate is no limit. Change
them while the grant runs with `UpdateLimits`, under the current fence:

```go
err := env.UpdateLimits(ctx, fence, execenv.Limits{
	DiskOps: execenv.TokenBucket{Rate: 2000},
})
```

Every `Env` has the whole contract below: fenced control, journal projection,
semantic observations, processes and revoke. There is nothing to discover
and no type assertion to make.

### Control

`Epoch()` identifies this exact occupancy and survives reconnect, re-Ensure
and a restart of the host daemon. Reconstruction generates a fresh epoch;
the host never recovers an old epoch without its highest fence.

Grants outlive the daemon. Each grant's machine runs on its own, and the
daemon records each grant's spec, epoch, control receipt and machine in a
state file it replaces atomically. A restarted daemon adopts every grant
whose machine still runs: same epoch, same fence, same processes. Each
process's output resumes from the offsets you held, and the process log
and observations you had not acknowledged are still there. A grant frozen
when the daemon went away is still frozen; it thaws under its next fence.
A grant whose machine ended while no daemon ran is lost after the restart
(`ErrLost` until you revoke it). The in-memory host's grants end with its
process.

```go
fence := execenv.ProjectionFence{
	ExecutionGrantID: env.ID(),
	EnvironmentEpoch: env.Epoch(),
	FenceRevision:    7,
}
receipt, err := env.Control(ctx, execenv.ControlRequest{Fence: fence, State: execenv.ControlFrozen})
if err != nil {
	return err
}
// Match receipt.Fence to the caller's current durable authorization before
// acknowledging the effect or opening any caller-side gate.
_ = receipt
```

The fence contains only the opaque grant ID, epoch, and positive revision
(maximum 2^53−1). Its meaning belongs to the caller. The host records the highest
intent before attempting an effect. Older requests and reuse of a revision with
a different state conflict. Exact successful retries return the same receipt;
exact failed retries attempt the effect again. `ControlStatus` reports the highest
intent and whether its effect was confirmed, including after a lost response.
Before the first fenced command its revision is zero, so enter the protocol with
a `running` request before the first projection call.

Control accepts `running`, `frozen`, and `revoked`. `frozen` blocks new I/O and
preserves the existing processes, their output and the tree; a later `running`
continues the same processes. The isolated host acknowledges `frozen` only after every monitor
thread is confirmed stopped; a sent signal alone is insufficient. Agent I/O
gating is separate from host execution suspension. A failed thaw keeps the local
gate closed and pauses the instance again. Any error requires reconciliation; it
is never evidence that the requested state was reached.

Revocation is terminal even if destruction initially fails. Failed destruction
keeps the grant's resources reserved until a successful retry. Administrative
`Host.Revoke(id)` destroys the current grant; `Env.Revoke` targets only the
captured epoch and cannot destroy a replacement. Process reads bind the
captured epoch; starts, input, resize and signals carry the fence.

### Seeds

When many grants start from the same tree, send it to each host once, as a
seed, instead of to every grant. Build an archive with package `archive`;
its digest names the tree, whatever order or compression it was written in:

```go
var buf bytes.Buffer
w, err := archive.NewWriter(&buf, archive.CodecZstd)
if err != nil {
	return err
}
_ = w.Dir("src")
_ = w.File("src/main.py", false, int64(len(main)), bytes.NewReader(main))
digest, err := w.Close()
if err != nil {
	return err
}
if _, err := host.PutSeed(ctx, digest, &buf); err != nil {
	return err
}
env, err := host.Ensure(ctx, execenv.Spec{ID: "job-7f3a", Image: "python", Seed: digest})
```

The host unpacks the archive once, refusing anything but directories and
regular files and any tree whose digest is not the one named, and keeps it as
a ready workspace disk. A grant created with the seed gets its own copy of
that disk: a reflink where the host's filesystem shares blocks, a local
block copy elsewhere, and nothing over the network. Putting a seed the host
already has returns at once. `Seeds` lists them and `DeleteSeed` removes one;
grants already created keep their copy. An unknown seed in a Spec is
`ErrNotFound`.

Initialize a seeded grant with its full manifest as usual. For a file the
seed already holds, give a reference with its size and digest and no
transfer id; upload only what differs. Paths the seed holds and the manifest
does not name are removed. A file neither seeded nor uploaded fails the whole
initialization with a `*MissingContentError` naming its paths, and changes
nothing.

### Files

Callers that keep no durable copy of their own can work on the workspace
directly. Reads name only the grant; changes carry its fence:

```go
info, err := env.WriteFile(ctx, fence, "out/report.txt", body,
	execenv.WriteOptions{ExpectedSHA256: execenv.ExpectAbsent})
entries, err := env.List(ctx, "out", 2, true) // with digests
rc, err := env.ReadFile(ctx, "out/report.txt", 0)
err = env.Import(ctx, fence, "", archiveReader) // all entries or none
rc, err = env.Export(ctx, []string{"out"}, "zstd")
```

`WriteFile` lands the file only once its whole body has arrived;
`ExpectedSHA256` makes it conditional on what is there now (`ExpectAbsent`:
nothing). `MakeDirectory`, `Move` and `Remove` do what their names say;
`Remove` needs `recursive` for a directory that is not empty. These are
guest changes like any other: once a projection is initialized they appear
in `Observe`. Bodies travel on the content connection.

A large write can survive a broken connection. Give it a `TransferID` of
your choosing: the bytes of a stream that breaks are kept under it, and you
carry on from where the host stopped:

```go
opts := execenv.WriteOptions{TransferID: "upload-42"}
_, err := env.WriteFile(ctx, fence, "data.bin", body, opts)
if err != nil {
	opts.Offset, err = env.TransferOffset(ctx, "upload-42")
	_, _ = body.Seek(opts.Offset, io.SeekStart)
	_, err = env.WriteFile(ctx, fence, "data.bin", body, opts)
}
```

A resume whose `Offset` is not what `TransferOffset` reports is
`ErrConflict`. The file still lands only once the whole body is there, and
then the transfer is used up. Kept bytes expire an hour after they last
grew.

### Project the workspace

Your application owns a journal of the workspace. The grant receives it as
consecutive journal entries, each bound to the current fence.

```go
body, err := env.UploadProjectionContent(ctx, fence, strings.NewReader("print('hello')\n"))
if err != nil {
	return err
}
_, err = env.InitializeProjection(ctx, execenv.ProjectionInitialization{
	Fence:           fence,
	MutationID:      "init-job-7f3a",
	WorkspaceCursor: 41, // your journal position this snapshot reflects
	Entries: []execenv.ProjectionEntry{
		{EntryID: "e-src", Kind: execenv.ProjectionDirectory, Path: "src"},
		{EntryID: "e-main", Kind: execenv.ProjectionFile, Path: "src/main.py", ContentVersion: "v1", Content: &body},
	},
	// Optional: leave dependency installs and build output out of Observe.
	// You choose the file; the host hardcodes none.
	Ignore: &execenv.IgnoreRules{
		File:     "your-ignore-file",
		Fallback: []string{"node_modules/", "__pycache__/", "*.pyc"},
	},
})
if err != nil {
	return err
}

body, err = env.UploadProjectionContent(ctx, fence, strings.NewReader("print('hello world')\n"))
if err != nil {
	return err
}
receipt, err := env.ApplyProjection(ctx, execenv.ProjectionApply{
	Fence:                  fence,
	MutationID:             "save-42",
	FromWorkspaceCursor:    41,
	ThroughWorkspaceCursor: 42,
	ExpectedHostCursor:     hostCursor, // from your last Observe page
	Mutations: []execenv.ProjectionMutation{{
		JournalCursor:           42,
		EntryID:                 "e-main",
		Operation:               execenv.ProjectionWrite,
		Kind:                    execenv.ProjectionFile,
		Path:                    "src/main.py",
		ExpectedContentVersion:  "v1",
		ResultingContentVersion: "v2",
		Content:                 &body,
	}},
})
```

Initialize once, before starting the first process, into an empty workspace.
Every Apply binds an exact grant/epoch/revision fence, mutation ID, from/through
journal cursors, expected host cursor, stable entry IDs, kinds, and opaque
content versions. The host compares versions for equality and never parses
them. Batches contain 1–128 consecutive mutations and at most 256 KiB of
metadata. Directory moves preserve all descendant identities and versions;
deleting a nonempty directory requires `Recursive`. Exact retries return their
original receipt; changing a used mutation ID conflicts. The epoch retains at
most 65,536 receipts and refuses additional mutations rather than forgetting
retry bindings.

An Apply whose expected host cursor is behind an unobserved guest change, or
that would overwrite a guest change on an affected path, fails with
`ErrConflict` before any path changes. Observe first, decide, then apply.
Freeze and thaw preserve the projection; neither requires a new snapshot.

File contents travel separately as immutable, scoped SHA-256 references. A
body streams to the grant's workspace disk as it arrives and is never held in
memory, so no size limit applies to it but the disk itself: a body or a tree
that would not fit is `ErrQuota`, and nothing is left half-written. Unconsumed
content expires after five minutes, with at most 512 handles per environment. Over the remote wire each
upload is one stream of 64-KiB messages on the content connection; one that
breaks off leaves nothing behind. References cannot name
paths, URLs, credentials, another environment or observation content.

Paths are POSIX-relative: no leading `/`, no `..`, no NUL, UTF-8.

### Observe guest changes

`Observe` is how the guest reports files **it** created, wrote, moved, or
deleted. Your own projection calls do not appear here.

```go
page, err := env.Observe(ctx, fence, acknowledged, 128)
if err != nil {
	return err // ErrLagged: resynchronize from your journal
}
for _, event := range page.Observations {
	if event.Content != nil {
		body, err := env.OpenObservationContent(ctx, fence, *event.Content)
		if err == nil {
			_, err = io.ReadAll(body) // the file as it is now
			_ = body.Close()
		}
		if errors.Is(err, execenv.ErrStale) {
			// Changed since: a later event of this entry reports it.
		} else if err != nil {
			return err
		}
	}
	mutation, accepted := decideInYourStore(event) // you own durable files
	if accepted {
		if err := env.ConfirmObservationProjection(ctx, execenv.ObservationProjection{
			Fence: fence, HostSequence: event.HostSequence, Mutation: mutation,
		}); err != nil {
			return err
		}
	}
	acknowledged = event.HostSequence
}
if err := env.AcknowledgeObservations(ctx, fence, acknowledged); err != nil {
	return err
}
hostCursor = page.HostCursor
```

Each ordered event binds its original projected cursor and version, an
epoch-stable node identity, and the size and SHA-256 of a created or written
file as it was observed. The body is not copied: `OpenObservationContent`
reads the file from the workspace as it is now, as one stream of 64-KiB
messages, hashing it as it goes. A file changed since then ends the stream
with `ErrStale` instead of its end, and a file gone or replaced fails with
`ErrStale` at once. That is safe to treat as "skip this one": every change
after an observation is a later observation of the same entry (a newer
write, a move or a delete), so reading the newest event of each entry
reaches the final state. Directory moves are single subtree operations,
parent creates precede children, and recursive directory deletion is
explicit.

After accepting a guest event durably, call `ConfirmObservationProjection` with
its host sequence and the exact resulting journal mutation before acknowledging
it. The host binds the original identity, topology and content digest to that
outcome. A later consecutive Apply of that mutation advances the baseline while
suppressing only that known echo, preserving any later guest writes. A
confirmation retry tolerates a new upload handle for the same content;
changing the outcome conflicts. Rejected or ignored outcomes need no
confirmation.

Acknowledge only after recording the outcome durably. Acknowledgement releases
the events, after which their content can no longer be opened; it does not
itself imply acceptance or advance projection versions. Unacknowledged
retention is bounded to 4096 events, and none of them holds a copy of a
body.
Overflow reports `ErrLagged` without discarding the retained prefix. A cursor
before the acknowledged prefix also returns `ErrLagged`.

Tool caches, virtual environments and dotfiles written through `$HOME` are not
in `/workspace` (see Run processes) and never appear here. A project-local `venv` or
`node_modules` inside `/workspace` is observed and counts toward the retention
bounds unless your `IgnoreRules` leave it out.

#### Ignore rules

`ProjectionInitialization.Ignore` names a workspace-relative file of
gitignore patterns, and a fallback list used while that file is missing, is
not a regular file, is larger than 64 KiB, or does not parse. Patterns follow
gitignore: `#` comments, `!` negation, a trailing `/` for directories only, a
leading or inner `/` anchors to the workspace root, `**` spans any depth, and
the last matching pattern wins. A path under an ignored directory cannot be
re-included.

- Patterns suppress only paths the projection does not hold. A path you
  projected, or one Observe already reported, stays observed while it exists,
  including after a move to an ignored name. A held path moved *into* an
  ignored, unheld directory leaves the stream as a delete.
- The ignore file and its parent directories are always observed. The guest
  re-reads the file whenever its bytes change, whether you projected the edit
  or the guest wrote it, and the next Observe applies it: paths it un-ignores
  arrive as creates.
- An ignored directory that is not held is never walked. Its contents cost
  nothing, never lag the stream, and never count toward the observation or
  entry limits.
- Ignored paths stay in the environment; nothing is deleted for being
  ignored. A projected file or move onto an ignored path replaces what the
  guest left there, as a checkout does, and a projected directory adopts an
  ignored directory already at its path.

The rules last for the epoch: initialization happens once, and an exact retry
is a replay.

The guest captures changes while the workload runs: `Observe` never freezes
it. Identities come from the filesystem itself (file handles that carry the
inode generation), not content hashes, so nothing is held open between
captures and a tree of any size costs no descriptors. One fanotify mark on
the workspace's filesystem names every path that changes, at any depth;
`Observe` reads again only those paths and what is under them, hashing only
files whose size or times changed, so its cost is the change's, not the
tree's, and an `Observe` with nothing new reads nothing. Every path that
changed before `Observe` was called is in what it reports. The whole
workspace is walked instead when the watcher cannot name what changed (its
queue overflowed), after the ignore file changed, and at least every 30
seconds, for writes through `mmap` that no event reports. A change still
under way when a capture passes it (a file being written, a directory being
moved or removed) is reported as far as it had got, and the next `Observe`
walks again to report the rest; a body read after it moved on is
`ErrStale`. Projection
calls (initialize and apply) still stop the workload behind a
kernel-acknowledged barrier while they change the workspace. The agent's
`-contain-workload` option requires a dedicated Linux guest with a usable
unified workload hierarchy. It launches unprivileged processes inside that
hierarchy before execution and prevents privilege escalation. Uncertain
filesystem commits remain contained. Baked images start a PID-1 child reaper,
require the kernel workload hierarchy, and run the contained agent; a guest
helper that cannot serve the journal is refused at Ensure. Run `make certify`
with the intended kernel/rootfs on real isolation hardware before production
use; it covers TLS remote journal projection, immutable guest events,
accepted-outcome echoes, semantic directory moves and preservation of the same
shell process through save and containment. Portable unit tests and Linux build
containers are not that certification.

### Process log

`ObserveProcesses` is an ordered log of every program the environment's
workload starts and how each process ends. The guest helper records it from
the guest kernel, outside the workload, so the workload cannot see, alter or
suppress an event already recorded. Each event leaves the guest as soon as it
is taken; the host keeps it until you acknowledge it.

```go
page, err := env.ObserveProcesses(ctx, fence, acknowledged, execenv.MaxProcessPage)
if err != nil {
	return err // ErrLagged: that position was already acknowledged
}
for _, event := range page.Events {
	switch event.Kind {
	case execenv.ProcessStarted:
		_ = event.Argv // bounded; Truncated says it was cut, Unread that it ended unread
		_ = event.Tag  // the Start tag for a process Start created, else ""
	case execenv.ProcessExited:
		_ = event.ExitCode // nil when event.Signal ended it
	case execenv.ProcessDropped:
		_ = event.Dropped // events lost before this one
	}
	record(event) // keep what you need before acknowledging
}
if err := env.AcknowledgeProcesses(ctx, fence, page.Next); err != nil {
	return err
}
acknowledged = page.Next
```

- The same fence as observations guards both calls; a stale epoch or
  revision is `ErrConflict`, a frozen grant `ErrFrozen`, and revoke ends the
  log with `ErrRevoked`.
- Only the contained workload's processes are recorded, the login shell
  included; never the guest helper or init. A fork that never starts a
  program is part of its parent. A process that replaces its program reports
  another start under the same PID, and its one exit ends the latest.
- Sequences are per epoch and gapless. An exit never precedes its start, but
  PIDs are reused, so pair starts and exits by order.
- `Dir` is the working directory when it is inside `/workspace` or the scratch
  home, else empty. A process that ended before its command line could be
  read is reported `Unread`.
- A process `Start` created reports its start with its `Tag`; the processes
  it starts in turn have none, so a shell's commands are told apart from the
  shell. The helper records the tag before the process runs its program, so
  the start is never seen untagged, and since it ran that command itself,
  such a start is never `Unread` either.
- A full log never evicts and never blocks the workload. It counts what it
  cannot keep, and one `ProcessDropped` event carrying the count takes the
  next sequence once acknowledgement frees space. If the guest kernel's own
  event buffer overflows, the loss is reported the same way, counted as 1
  because the kernel does not say how many.
- A cursor before the acknowledged prefix is `ErrLagged`; acknowledging or
  asking past the events produced is `ErrInvalid`.

The guest kernel must have the process events connector
(`CONFIG_CONNECTOR`, `CONFIG_PROC_EVENTS`; `scripts/check-kernel-config`
requires both). A guest helper without it says so in its handshake and is
refused at Ensure, like one that cannot serve the journal. The in-memory
adapter's shell records itself and every command line it runs.

### Run processes

A grant runs processes by tag: up to 32 at once. A shell is a process with
a terminal; a command is one without. Each tag is the caller's and unique
among the grant's running processes; every other call names the process
by it. A process lives in the grant, not on the connection that started
it, and its output is kept on the host for any number of readers.

```go
// A shell: an empty Argv with a terminal is the image's login shell.
_, err := env.Start(ctx, fence, execenv.ProcessSpec{
	Tag:         "sh",
	PTY:         &execenv.Window{Cols: 120, Rows: 40},
	DetachGrace: time.Minute, // end it once nothing has been attached for a minute
})

// A command: separate stdout and stderr, its own exit code.
_, err = env.Start(ctx, fence, execenv.ProcessSpec{
	Tag:     "test",
	Argv:    []string{"go", "test", "./..."},
	Dir:     "src",
	Env:     map[string]string{"CGO_ENABLED": "0"},
	Timeout: 10 * time.Minute, // TERM, then KILL five seconds later
})
```

Read output by attaching. Each event carries its stream and absolute byte
offset; remember the offsets you have read and attach from them after a
lost connection to get exactly what followed:

```go
a, err := env.AttachProcess(ctx, "sh", map[execenv.Stream]uint64{execenv.StreamPTY: offset})
defer a.Close() // detach; the process keeps running
for {
	out, err := a.Next(ctx)
	if errors.Is(err, io.EOF) {
		break // after OutputExit
	}
	switch out.Kind {
	case execenv.OutputData:
		offset = out.Offset + uint64(len(out.Data))
	case execenv.OutputTruncated:
		// Bytes from out.Offset to out.To fell out of the history.
	case execenv.OutputExit:
		// out.Exit.Code, or out.Exit.Signal
	}
}
```

Input is by offset too. `WriteInput` appends at the offset you give and
returns the new one; bytes the process already took are dropped, so a
resend after a lost response types nothing twice, and an offset past what
it took is `ErrConflict`. `ProcessInfo.Input` says where input stands.

```go
next, err := env.WriteInput(ctx, fence, "sh", input, []byte("make\n"))
err = env.Resize(ctx, fence, "sh", execenv.Window{Cols: 80, Rows: 24})
err = env.SignalProcess(ctx, fence, "sh", execenv.SignalInterrupt)
err = env.CloseInput(ctx, fence, "test") // a command started with Stdin
```

- Streams are `pty` for a terminal, else `stdout` and `stderr`. Each keeps
  `HistoryBytes` (1 MiB by default, at most 4 MiB). Attaching without an
  offset for a stream starts at its oldest byte still held; an offset past
  its end is `ErrInvalid`.
- A command's input is closed at once unless `Stdin` is set.
- `Process` and `ListProcesses` describe processes, running or exited.
  An exited process's output stays readable until its tag is started
  again. Starting a tag that is still running is `ErrConflict`; a 33rd
  running process is `ErrBusy`.
- Signals are `INT`, `TERM`, `KILL`, `HUP` and `QUIT`, sent to the
  process's whole session.
- Freeze refuses input and starts (`ErrFrozen`) and keeps every process.
  Revoke ends them (`ErrRevoked`); a lost grant ends them with `ErrLost`.
- Over a remote connection each attachment has its own 256 KiB flow
  control window: a stalled reader holds back only its own output, never
  calls or other attachments.

Zero Cols or Rows means 80×24.

`Screen` is a terminal process's screen as its terminal shows it now. The
guest helper keeps a headless terminal for each one, fed every byte the
process writes, so even a full-screen program reads back exactly:

```go
s, err := env.Screen(ctx, "sh")
_, _ = os.Stdout.Write(s.Data) // redraws it on a blank terminal of s.Window
a, err := env.AttachProcess(ctx, "sh", map[execenv.Stream]uint64{execenv.StreamPTY: s.Offset})
```

`Data` redraws the screen's text and styles, the cursor and whether the
program has the alternate screen; `Offset` is the output it reflects, so
attaching from it continues the screen with nothing missed or repeated.
`Text` is each row as plain text, and `AltScreen`, `CursorRow`, `CursorCol`
and `CursorHidden` say the rest. While a full-screen program has the
alternate screen, the main screen behind it is not included. A process
without a terminal is `ErrInvalid`; an exited one's screen stays readable
until its tag is started again.

A process starts in `/workspace` (`execenv.GuestWorkspace`), the projected
tree, or `Dir` within it. Its `$HOME` is `/run/execenv/home`
(`execenv.GuestHome`), guest-local scratch outside the tree: it is not
projected, observed, or harvested, and it is discarded with the grant. Both
live on the grant's writable disk, not in guest memory.

### Revoke

```go
err := host.Revoke(ctx, "job-7f3a") // also env.Revoke(ctx); both are idempotent
```

The guest is destroyed. Keep anything you still need in your own store
before this call.

### Lost grants

A grant's environment can end without a revoke: its machine exits, its
guest kernel panics, or the host loses the guest link for good. The host
notices on its own. It frees the grant's resources and cleans up as a revoke would, and
every call on that occupancy fails with `ErrLost`: processes and their
attachments, Control, ControlStatus, projection, Observe and the process
log. Over the wire the same sentinel reaches the
client.

`Ensure` of the lost id also returns `ErrLost`, so a reconnecting caller
cannot mistake an empty new guest for the one it had. Acknowledge the loss
with `Revoke` (host or env; it returns nil), then `Ensure` the id again: it
gets a new `Epoch`, and you rebuild it from your journal as after any new
occupancy. Fences from the lost epoch never apply to the new one.

### Follow grants

A host lists its grants and tells you when they change, so nothing polls:

```go
grants, err := host.ListGrants(ctx)       // held and lost grants, with state
g, err := host.WaitGrant(ctx, "grant-1")  // once it has left GrantCreating
w, err := host.WatchGrant(ctx, "grant-1") // its state now, then each change
for {
	g, err := w.Next(ctx)
	if err == io.EOF {
		break // after GrantRevoked
	}
	_ = g.State // creating, running, frozen, suspended, revoked or lost
}
hw, err := host.WatchHost(ctx) // every grant and the capacity, then changes
```

`GrantInfo` carries the grant's epoch, image, network, resources, labels,
state, the fence revision of the control that put it there, and when it
started. A watch holds at most the latest state of each grant it has not
read yet: a burst of changes arrives as the last of them, so a slow reader
costs the host nothing more. An unknown grant is `ErrNotFound`.

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
| `ErrInvalid` | bad id, image, path, fence, or config |
| `ErrUnknownImage` | that catalog id is not on the host |
| `ErrCapacity` | the grant does not fit; `errors.As` a `*CapacityError` for the short resource |
| `ErrDraining` | host is draining; it serves held grants only |
| `ErrConflict` | live grant mismatch, stale fence, or unobserved guest change |
| `ErrUnavailable` | host unusable, or it refused the client's credentials |
| `ErrFeature` | the host does not offer a feature this client needs |
| `ErrFrozen` | I/O while frozen |
| `ErrRevoked` | grant is gone |
| `ErrLost` | grant's environment ended without a revoke; Revoke, then Ensure again |
| `ErrBusy` | the grant already runs 32 processes |
| `ErrClosed` | attachment or content reader closed, or input to a process that exited (not revoke) |
| `ErrNotFound` | path missing from the projection, or no process has that tag |
| `ErrLagged` | observations no longer retained, or a process log position already acknowledged |
| `ErrStale` | an observed file changed after it was observed; read its later event |
| `ErrTooLarge` | metadata over its bound, or too many content handles |
| `ErrQuota` | the grant's workspace disk is full; the call changed nothing |
| `ErrConnection` | remote session lost |
| `ErrNetwork` | allowlist requested, host offers none |

Do not log tokens, tree bodies, or process input and output.

### Limits

| Cap | Value |
| --- | --- |
| Grant id / image id | 1–64 characters `[A-Za-z0-9._-:]` |
| File bodies, one or all | the grant's `DiskBytes` |
| Mutations per Apply | 128 |
| Apply metadata | 256 KiB |
| Initialization metadata | 64 MiB (a few hundred thousand entries) |
| Unacknowledged observations | 4096 events |
| Unacknowledged process events | 4096 events, 2 MiB |
| Process log page | 256 events, 256 KiB |
| Started command line | 64 arguments, 1 KiB |
| Reported working directory | 4096 bytes |
| Ignore file read | 64 KiB (larger uses the fallback) |
| Ignore fallback | 256 patterns, 16 KiB |
| Content and process output per message | 64 KiB |
| HTTP/2 stream window, client receiving | 256 KiB interactive, 4 MiB content |
| HTTP/2 stream window, host receiving | 4 MiB |
| Path length | 4096 bytes UTF-8 |
| Path segments | 64 |
| One segment | 255 bytes |
| Running processes per grant | 32 |
| Output history per stream | 1 MiB by default, 4 MiB at most |
| Process tag | 1–64 characters `[A-Za-z0-9._-:]` |

The workspace inside the guest is `/workspace`; `$HOME` is
`/run/execenv/home`. Both share the grant's workspace disk, its
`Resources.DiskBytes`.

---

## Create a host

This section is for the Linux machine that will occupy grants. It does
not use Go. You install binaries and catalog disks, then give the
application the listen address, token, and certificate.

### The isolation boundary

Each grant is a microVM run by the runtime and supervisor pair bootstrap
installs: a guest that fully compromises its own kernel still has to escape a
hypervisor. A host without a usable `/dev/kvm` fails closed: bootstrap writes
nothing and `Ready` reports unusable. There is no container fallback.

### What the machine needs

- Linux on **amd64**
- Root, or equivalent, for the default layout
- A usable isolation device at `/dev/kvm` (readable and writable by the
  host process, typically the `kvm` group)
- The microVM runtime and supervisor pair (bootstrap installs them)
- `mkfs.ext4` and `resize2fs` (e2fsprogs): every grant gets a sparse ext4
  workspace disk, and every seed is kept as one
- `ip` and `iptables` if you configure an allowlist
- Network reachability from the application on port **8443** (or the
  listen address you choose)

Missing prerequisites fail closed. Docker is not used on this machine and is
not a substitute.

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

### Grant resources

Grants ask for their own size; the Host configuration says how much the host
gives in all and what a grant gets by default:

| Key | Default | Meaning |
| --- | --- | --- |
| `capacity` | measured by bootstrap | `cpu_millis`, `memory_bytes`, `disk_bytes` the host gives grants in all |
| `cpu_overcommit` | 1 | multiplies the CPU total; memory and disk are never overcommitted |
| `grant_defaults` | 1000 millis, 512 MiB, 2 GiB | a grant's size where its Spec leaves one unset |
| `grant_max` | the capacity | the most one grant may ask for |
| `mkfs` | `mkfs.ext4` on `PATH` | formatter for workspace disks |
| `jail_uid_base` | 2000000000 | first of 65536 host uids reserved for jailed microVM monitors |

A grant's machine gets enough virtual CPUs for its `cpu_millis`; its cgroup
throttles it to exactly that, with no swap. Its memory is held at its
`memory_bytes` plus the monitor's own overhead: past that the kernel
reclaims the host's cache of the grant's disk and slows its writes, and
only 256 MiB further does it stop the machine. Its workspace disk and
network device carry the grant's rate limits, changeable while it runs.

Each grant's monitor runs in its own jail under
`<state>/work/jails`, as its own unprivileged uid from the
`jail_uid_base` range, never as root. The kernel and rootfs are hard-linked
into the jail (read-only bind mounts when they sit on another filesystem),
so catalog files must stay readable by other users (the installed mode is
`0644`). Reserve the uid range for execenv: a restarted host kills every
process running under it except the machines of the grants it adopts.

The workspace disk is a sparse file in the grant's directory, so only
written blocks use host storage. It is created when the grant starts and
removed on revoke, and when a restarted daemon finds a grant's machine
gone. The guest does not reclaim space from deleted files during a grant.

### Templates

A grant can start from a saved machine instead of booting. List the
machine sizes to save in Host configuration:

```json
"shapes": [
  {"name": "small", "vcpus": 1, "memory_bytes": 536870912},
  {"name": "large", "vcpus": 4, "memory_bytes": 4294967296}
]
```

For every catalog image and shape the daemon builds a template in the
background, one at a time: it boots the image at that size with a
placeholder disk, waits until the guest is idle before it has touched any
grant's disk, and saves it. A host that offers network access (`allow`)
saves each one a second time with a network device, on a placeholder link
of its own; the guest has no address on it yet. `Ready` lists each
template, whether it has a network device, and whether it is ready. A
grant whose memory is exactly a shape's, and whose CPU needs no more
virtual CPUs than the shape has, then starts from the template of its image
with the fewest such CPUs, with a network device exactly when the grant has
network access; its CPU is still throttled to what it reserved, and its
disk and network rate limits are its own. Any other grant, and every grant
until its template is ready, boots at its exact size.

A restored machine is made one grant's before anything in it runs. Guest
init waits for a start message from the host carrying fresh entropy, the
time, a machine id, the grant's epoch and, with network access, the
grant's address; it reseeds the random generator, sets the clock and the
machine id, gives the network device the grant's address and its default
route through the grant's own link, and only then mounts the grant's own
disk and starts the helper. A booted guest is configured by the same
message, so the two start alike. A restored network grant's device is
backed by the grant's own link from the moment it is loaded. So two grants from one template share no
randomness, identity or files. Each restored grant maps the template's
memory privately: pages it writes are its own. Templates live in
`<state>/work/templates`; a changed image or shape is built again.

The daemon watches every grant's machine. If a monitor exits, a guest
panics (guests boot with `panic=1`, so a panic resets and ends the
machine), or the guest link breaks, the daemon releases that grant's jail,
disk, tap and uid at once, as a revoke would, and its resources are free again.
Callers see `ErrLost`; `Ready` counts it in `Lost`.

Each microVM also gets a balloon device with free page
reporting and `deflate_on_oom`, so pages the guest frees go back to the
host and a grant's resident memory follows what it actually uses. This
needs the guest kernel to have `CONFIG_VIRTIO_BALLOON`,
`CONFIG_MEMORY_BALLOON` and `CONFIG_PAGE_REPORTING` built in; without
them the device is inert and grants behave as before. Restoring templates
and suspended grants needs `CONFIG_VMGENID`. CI checks a guest kernel's
`.config` with `scripts/check-kernel-config`.

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
| `--cpu-millis` | every CPU, 1000 each |
| `--memory-bytes` | the machine's memory, less 1 GiB or a tenth for the host |
| `--disk-bytes` | 90% of the free space under `--state` |
| `--device` | `/dev/kvm` |
| `--mkfs` | `mkfs.ext4` on `PATH` |

What bootstrap does, in order:

1. Opens the isolation device. If that fails, it stops and writes
   nothing. The error says the device is missing or not usable, and that
   there is no container fallback.
2. Finds `mkfs.ext4`, or stops and writes nothing.
3. Creates the bin, state, and config directories.
4. Installs the `execenv` daemon binary (from this tag, or `--execenv`
   for an air-gapped file).
5. Installs the runtime and supervisor pair, fetching them if they are
   not already on `PATH` (unless `--no-fetch`).
6. Writes Host configuration and TLS files (see below).
7. Writes `execenv.service` and enables it (unless `--no-start`).

The catalog is still empty. The host cannot occupy grants until you
install at least one Image.

```sh
sudo execenvctl status
# device=ok
# mkfs=ok
# unit=active
# drain=serving
# grants=0
# installed=
# release=v1.2.3
```

`drain` is `serving`, `draining`, `drained`, or `unreachable` when the
daemon does not answer on its operator socket; `grants` is how many grants
it holds.

The capacity flags override what bootstrap measures; a re-run without them
keeps the capacity already written. Useful flags: `--listen`,
`--execenv ./execenv`, `--no-start`,
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

Mutual TLS is also supported. Set `tls_client_ca` in Host configuration to a
PEM CA that verifies caller certificates. With a token configured, either a
valid token or a verified certificate authenticates the caller. With an empty
token, a verified client certificate is required. TLS defaults to version 1.3.

For a public hostname, a typical client config is: listen
`0.0.0.0:8443`, replace `tls.crt` / `tls.key` with a cert whose SAN is
that hostname, set the application’s `ServerName` to the same name, and
open 8443 on the firewall only to the application network.

The daemon takes an exclusive lock on `work_dir`. A second daemon cannot use
the same state directory. `work_dir` must be on a filesystem that allows
device nodes (not one mounted `nodev`, as a tmpfs `/tmp` often is): each
jail holds its own isolation device, and the daemon refuses grants
otherwise. Grants' machines outlive the daemon (see [Control](#control)), so
startup adopts the ones its state files name and stops only the jails
nothing names; revoke removes the grant directory.

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
restarts the daemon. The restarted daemon adopts its grants, so they keep
running. `remove` refuses an image a live grant was started from; revoke
those grants first.

Ids you can install when the release includes them: `python`, `node`,
`go`, `java`. `default` is a large universal-class disk, often published
onto the same tag later. `fixture` is a tiny disk for live isolation
tests.

Prefer the slim per-language ids for high-concurrency use.
Every grant reads its image from the host page cache, and a slim image
keeps that cache, boot time and install size small. `default` is several
gigabytes of toolchains most grants never touch; install it only for
grants that genuinely need a multi-language environment.

Disks baked before the per-grant workspace disk keep the old guest init:
their `/workspace` stays in guest memory and doubles as `$HOME`. Reinstall
(or `execenvctl upgrade`) to get the current guest init.

```sh
sudo execenvctl remove python   # does not touch the token
```

Until an id is installed, `Ensure` of that id is `ErrUnknownImage`.

### Drain the host

`install`, `remove` and `upgrade` restart a running daemon, and the
restarted daemon adopts every grant it held. To take a host out of service
instead, drain it:

```sh
sudo execenvctl drain --timeout 30m
# state=draining grants=3
# state=draining grants=1
# state=drained grants=0
```

While draining, the daemon keeps serving the grants it holds (including
`Ensure` of one of them) and refuses new ones with `ErrDraining`; `Ready`
reports `Usable` false with reason `draining`, so the application places
new grants elsewhere. `drain` prints the remaining count as it changes and
returns once none remain. If `--timeout` passes first it exits non-zero
and the daemon stays draining; `--timeout 0` starts the drain and returns
at once. To cancel a drain without restarting:

```sh
sudo execenvctl undrain
```

A daemon whose grants would not survive a restart (the in-memory adapter)
is not restarted while it holds any; `--force` restarts it anyway and
destroys them. A running unit whose daemon does not answer is refused too,
since then nothing can say what it holds.

execenvctl reaches the daemon on a Unix socket, `operator_socket` in Host
configuration (bootstrap writes `/var/lib/execenv/operator.sock`, mode
`0600`). It carries only drain requests, the live grant count and the
images those grants run from, never grant operations, so grant callers on
the network cannot take the host out of service. Drain state is not saved:
every daemon start begins serving.

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
matches, then restarts the daemon once. Live grants keep running: the
upgraded daemon adopts them, and their machines keep the guest agent they
started with until they end.
Token, TLS files, listen address, capacity, and allowlist stay.
If an installed id is missing from the new index, upgrade fails before
binaries move. When everything already matches: `upgraded=already current`.

---

## This repository

```sh
make check    # generated code current, test, race, vet
make generate # rewrite internal/gen from proto/ (commit the result)
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
