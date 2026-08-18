// Package agent is the guest-side helper the isolated host dials after a
// grant starts. It owns the home directory and the login shell.
//
// The host never implements a fake PTY. Unit tests run this package on a
// Unix socket; a real machine uses the same protocol on the guest link.
package agent
