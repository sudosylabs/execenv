// Package agent is the guest-side helper the isolated host dials after a
// grant starts. It owns the home directory and the login shell.
//
// The host never implements a fake PTY. Unit tests run this package on a
// Unix socket; a real machine uses the same protocol on the guest link.
//
// Closing the PTY ends the guest shell. The host keeps terminal sessions
// alive across client detaches above this package and closes the PTY only
// when it hangs a session up.
package agent
