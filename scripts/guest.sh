#!/bin/sh
# PID 1 for a baked catalog image booted as a microVM. The disk root is
# read-only. The host attaches a per-grant sparse writable disk as the
# second drive; the projected tree (/workspace, the working directory) and
# the shell's scratch $HOME both live on it, never in guest RAM. A grant
# never needs the network or a package manager.
#
# Fail closed: without the writable disk the guest does not start, rather
# than silently falling back to memory.
mount -t proc proc /proc 2>/dev/null
mount -t sysfs sysfs /sys 2>/dev/null
mount -t devtmpfs devtmpfs /dev 2>/dev/null
mount -t tmpfs -o size=16m,mode=0755,nosuid,nodev tmpfs /run || exit 1
mkdir -p /run/execenv/disk /run/execenv/home || exit 1
mount -t ext4 -o noatime,nosuid,nodev,noinit_itable /dev/vdb /run/execenv/disk || exit 1
mkdir -p /run/execenv/disk/workspace /run/execenv/disk/home || exit 1
mount -o bind /run/execenv/disk/workspace /workspace || exit 1
mount -o bind /run/execenv/disk/home /run/execenv/home || exit 1
export HOME=/run/execenv/home
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
mount -t cgroup2 cgroup2 /sys/fs/cgroup 2>/dev/null || exit 1
cd /workspace || exit 1
exec /usr/local/bin/execenv guest-init -home /workspace -scratch /run/execenv/home -contain-workload
