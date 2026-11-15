// Package agent is the guest-side helper the isolated host dials after a
// grant starts. It owns the home directory and the login shell.
//
// The guest link is one connection carrying the schema in
// proto/execenv/guest/v1, spoken with Connect over HTTP/2 with prior
// knowledge. The helper serves it; Client is the host's end. The schema is
// internal and versioned with the helper baked into each image: Verify
// refuses a helper on another protocol. The helper outlives any one link:
// a host that restarts dials again, its new link takes over from the old,
// and it finds the workload, its processes, their recent output and the
// process log as they were.
//
// Unit tests run this package over an in-memory pipe; a real machine uses
// the same protocol on the guest link. The helper runs each process the
// host starts, with or without a terminal, keeps each output stream's
// recent bytes by offset, and a headless terminal per terminal process for
// its screen.
//
// The helper records the contained workload's processes from the guest
// kernel's process events, outside the workload, and streams them to the
// host as soon as they are taken. A bounded queue between the kernel and
// the link counts what a slow link cannot carry, so the workload never
// waits on the log. The Client retains the events on the host until the
// caller acknowledges them. A helper whose kernel lacks process events says
// so in its handshake, and the host refuses it.
package agent
