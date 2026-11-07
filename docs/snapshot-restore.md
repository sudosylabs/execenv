# Snapshot restore for `vm` grants: design, deferred

Status: investigated, not implemented. Every `vm` grant still cold-boots its
guest kernel and init. This note records why, and what has to be true before
restore from a per-image pre-booted snapshot is safe.

## What restore would buy

A per-image snapshot taken after the guest kernel and init have booted, with
its memory file loaded through the `File` memory backend, is mapped
`MAP_PRIVATE`: pages load on demand, writes go to anonymous copy-on-write
memory, and every grant of the image shares the clean pages through the host
page cache. Boot latency drops from kernel boot plus init to page faults, and
resident memory per grant falls to the pages that grant actually dirtied.

## Why it is deferred

Each item below is a requirement from the monitor's snapshot documentation
(`docs/snapshotting/snapshot-support.md` upstream) checked against this
repository's launcher.

1. **One snapshot, many grants is the documented insecure case.** Upstream
   considers resuming the same snapshot in more than one microVM insecure,
   because everything in guest memory is duplicated: unique identifiers,
   cached random numbers, and cryptographic state. The secure pattern is a
   snapshot resumed once. A per-image template resumed for every grant is
   exactly the case the documentation warns about, so the guest must be
   designed to hold no secret or identity at snapshot time (below).
2. **Entropy.** The VMGenID device is always present and its generation ID
   changes on resume. Linux 5.18 and later (with ACPI) reseed the in-kernel
   PRNG from it. This repository does not control the guest kernel: it comes
   from the `KERNEL_URL` CI secret. Its version and `CONFIG_VMGENID` are
   unknown here. User-space state (the Go runtime's seeds in the guest agent,
   anything the init generated) is not reseeded by VMGenID at all.
3. **Disk paths.** Block device backing files must be reachable at the same
   relative paths for the restoring process as for the snapshotting one, and
   there is no drive-path override in the documented load API. The per-grant
   workspace disk therefore has to sit at one fixed path inside each grant's
   jail. The current launcher passes absolute host paths and does not place
   the kernel, rootfs, machine config, disk or vsock socket inside the jail's
   chroot. That has to be fixed first, independently of snapshots.
4. **Network.** Guest connectivity is not guaranteed after resume, and the TAP
   devices the snapshot used must exist for the new process. Per-grant TAP
   names differ, so each restored grant would need its own network namespace
   holding a TAP with the snapshot's fixed name, and the guest would need its
   IPv4 address reconfigured after resume. No network override is documented
   in the snapshot guide.
5. **Host link.** Open guest-link connections are closed on resume, while
   listening sockets survive and take the restored machine's CID. Because the
   host side of the guest link is a per-machine Unix socket, CIDs do not have
   to be unique across grants. The agent must be in `Accept` (not mid-session)
   when the snapshot is taken.
6. **Balloon.** Free page reporting can only be enabled before boot, so the
   snapshot must be taken from a machine configured with the balloon device
   described in the README. Statistics settings also cannot change across
   restore.

## Design, when the prerequisites hold

1. **Jail layout.** Populate each grant's chroot with hard links (or bind
   mounts) of the kernel, rootfs and snapshot files, plus the grant's own
   workspace disk, all at fixed relative paths. Run the monitor as an
   unprivileged per-grant uid instead of the daemon's.
2. **Template per image.** After `execenvctl install`, boot the image once
   with a placeholder workspace disk and no NIC. Guest init mounts `/proc`,
   `/sys`, `/dev`, `/run` and the cgroup hierarchy, then blocks on the guest
   link waiting for a start message. It must not mount the workspace disk,
   open the workload hierarchy, generate identifiers, or start the agent
   before that message. Pause, snapshot (full), and store
   `snapshot`/`memory` beside the image, hashed into the catalog entry so
   `Ready` never offers a template that does not match its rootfs.
3. **Restore per grant.** Create the grant's jail, sparse workspace disk and
   (for allowlist) network namespace; load the template with the `File`
   memory backend and `resume_vm`; send the start message carrying the grant's
   network configuration. Only then does guest init mount the workspace disk
   and exec the agent, so every grant-specific secret, cursor epoch and
   workload hierarchy is created after the fork point.
4. **Fail closed.** If the template is missing, its hash is stale, or the
   kernel does not report a VMGenID-capable configuration
   (`scripts/check-kernel-config` warns on `CONFIG_VMGENID`), fall back to a
   cold boot of the same image. That fallback stays within the `vm` class; it
   is a slower path, not a weaker boundary.
5. **Certify.** Extend `make isolation` with a test that restores two grants
   from one template and checks that their guest `/proc/sys/kernel/random/uuid`
   values, agent epochs and workspace contents are independent.

## Image size, separately

The other cold-boot cost is the image itself. `scripts/bake` defaults to the
universal Codespaces-class source, and the `default` catalog id is that
multi-gigabyte disk. The release workflow already bakes only the slim
per-language ids, with `default` published separately. The recommendation,
documented in the README, is to keep `default` but treat the slim
per-language ids as the default for exams and other high-concurrency use:
they shrink page-cache pressure, install size and, once restore exists,
template size.
