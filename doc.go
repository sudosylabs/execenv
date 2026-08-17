// Package execenv occupies isolated execution grants on one host.
//
// Callers ensure a grant, attach one PTY, freeze and thaw I/O, and revoke.
// Isolation machinery stays behind the contract.
package execenv
