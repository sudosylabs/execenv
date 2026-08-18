// Package bake turns a container filesystem into an on-host catalog
// image: a kernel, a root filesystem, and the SHA-256 digest Ready
// expects (kernel file then rootfs file).
//
// Bake is an operator or CI step. It is not part of Ensure. The daemon
// does not pull containers and does not import this package.
package bake
