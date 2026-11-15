// Package agent is the guest-side helper the isolated host dials after a
// grant starts. It owns the home directory and the login shell.
//
// The guest link is one connection carrying the schema in
// proto/execenv/guest/v1, spoken with Connect over HTTP/2 with prior
// knowledge. The helper serves it; Client is the host's end. The schema is
// internal and versioned with the helper baked into each image: Verify
// refuses a helper on another protocol. The link is never redialled, so its
// end is the helper session's end.
//
// The host never implements a fake PTY. Unit tests run this package over an
// in-memory pipe; a real machine uses the same protocol on the guest link.
//
// Closing the PTY ends the guest shell. The host keeps terminal sessions
// alive across client detaches above this package and closes the PTY only
// when it hangs a session up.
//
// The helper records the contained workload's processes from the guest
// kernel's process events, outside the workload, and streams them to the
// host as soon as they are taken. A bounded queue between the kernel and
// the link counts what a slow link cannot carry, so the workload never
// waits on the log. The Client retains the events on the host until the
// caller acknowledges them. A helper whose kernel lacks process events says
// so in its handshake, and the host refuses it.
package agent
