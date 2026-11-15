#!/bin/sh
# PID 1 for a baked catalog image booted as a microVM, first stage. The
# disk root is read-only. This stage mounts only what does not belong to a
# grant, then hands PID 1 to guest-init, which waits for the host's start
# message before it touches the grant's disk. That is what lets a machine
# saved at that point (a template) become any grant's: init reseeds the
# random generator, sets the clock and the machine id, then mounts the
# per-grant sparse writable disk (the second drive) and binds the projected
# tree (/workspace, the working directory) and the shell's scratch $HOME
# from it, never from guest RAM. A grant never needs the network or a
# package manager.
#
# Fail closed: without the writable disk the guest does not start, rather
# than silently falling back to memory.
mount -t proc proc /proc 2>/dev/null
mount -t sysfs sysfs /sys 2>/dev/null
mount -t devtmpfs devtmpfs /dev 2>/dev/null
# Terminals: a process's terminal end is opened from here.
mkdir -p /dev/pts || exit 1
mount -t devpts -o newinstance,ptmxmode=0666,mode=0620 devpts /dev/pts || exit 1
ln -sf pts/ptmx /dev/ptmx || exit 1
mount -t tmpfs -o size=16m,mode=0755,nosuid,nodev tmpfs /run || exit 1
mkdir -p /run/execenv/disk /run/execenv/home || exit 1
export HOME=/run/execenv/home
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
mount -t cgroup2 cgroup2 /sys/fs/cgroup 2>/dev/null || exit 1
cd / || exit 1
exec /usr/local/bin/execenv guest-init -home /workspace -disk-home /run/execenv/disk/workspace -scratch /run/execenv/home -content /run/execenv/disk/content -contain-workload
