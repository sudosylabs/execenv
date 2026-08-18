#!/bin/sh
# PID 1 for a baked catalog image. The disk root is read-only. Home is a
# tmpfs so a grant never needs the network or a package manager.
mount -t proc proc /proc 2>/dev/null
mount -t sysfs sysfs /sys 2>/dev/null
mount -t devtmpfs devtmpfs /dev 2>/dev/null
mkdir -p /workspace
mount -t tmpfs -o size=2G tmpfs /workspace 2>/dev/null
export HOME=/workspace
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
exec /usr/local/bin/execenv agent -home /workspace
