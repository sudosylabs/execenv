# execenv

execenv occupies isolated execution grants on one host.

## Language

**Host**:
One machine that can occupy grants. One client value talks to exactly one
host.
_Avoid_: Cluster, pool, hypervisor

**Host configuration**:
The operator document that makes one machine a Host: how it listens, its
catalog Images, slots, Network dests, and isolation paths. It is not the
grant contract and is not imported by grant callers.
_Avoid_: host.json, written config, daemon settings

**Grant**:
The caller-chosen occupancy of at most one isolated environment on one host.
_Avoid_: Session, virtual machine

**Environment**:
The isolated, non-authoritative projection attached to one grant, running
the grant's processes.
_Avoid_: Sandbox, local folder

**Process**:
A program a caller starts in an environment, named by the caller's tag,
with or without a terminal. Its output is kept on the host per stream by
absolute offset, so any number of readers attach and resume; its input is
taken by offset, so a resend duplicates nothing. A shell is a process with a
terminal. Exit, signal, timeout, detach grace and revoke end it.
_Avoid_: Terminal (for the process), session, job

**Port connection**

A bidirectional byte stream from a caller to a TCP port on its grant's own
loopback address.

_Avoid_ “port forwarding”, “ingress”, “tunnel”.

**Seed**:
A workspace a caller stores on the host once, by its archive's digest, so
many grants start from a copy of it instead of a projection each.
_Avoid_: Snapshot (for the workspace), image

**Template**:
A saved machine of one image and one shape, idle before any grant's disk,
that grants start from instead of booting. Each restored grant gets its
own entropy, clock, machine id and disk from its start message.
_Avoid_: Snapshot (for the template), warm pool

**Shape**:
An operator-chosen machine size, virtual CPUs and memory, that templates
are saved in. A grant uses one only when its memory is exactly the shape's.
_Avoid_: Slot, flavor

**Suspend**:
What an idle grant can become: its running machine saved to the host's
disk and stopped, only its disk reserved, until the caller's thaw restores
it.
_Avoid_: Hibernate, pause (for the saved state)

**Cursor**:
An opaque acknowledged position in one Environment's bounded stream of
guest-originated file changes. It resumes that stream after reconnect; it is
not a file version or durable storage position.
_Avoid_: Offset, revision, sequence number

**Image**:
A named, already-on-host guest disk: one kernel plus one root filesystem
with the toolchain baked in. It is not a container tag and is not pulled
when a grant starts. CI produces it with `scripts/bake`. Operators
install a catalog id with `execenvctl`; they do not bake on the host.
_Avoid_: Docker image, devcontainer, live package install

**Manager**:
The operator tool (`execenvctl`) that bootstraps a host and later
installs catalog disks. It is not the daemon and is not imported by
callers of the module.
_Avoid_: nvm, host API

**Agent**:
The guest-side helper that owns the projected workspace, the scratch home
and the login shell.
The host dials it after a grant starts. It is not a fake in-process PTY.
_Avoid_: SSH, container sidecar

**Network**:
A grant either has no NIC or the host's IPv4 allowlist. Destinations are
operator-configured on the host; the grant caller cannot add any.
_Avoid_: Guest firewall, caller dests

**Isolation class**:
The kind of boundary a Host puts around each grant, reported in Ready:
`vm` (a microVM with its own guest kernel) for every isolated host, or
`none` for the in-memory test adapter. There is no weaker fallback.
_Avoid_: Container fallback, adapter type

**Scratch home**:
The shell's `$HOME` inside the environment, outside the workspace. It is
never projected, observed or harvested, and is discarded with the grant.
_Avoid_: Home directory (for the workspace)
