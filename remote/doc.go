// Package remote serves the execenv Host contract over the network, and
// dials it.
//
// The wire is the protobuf schema in proto/execenv/v1, spoken with Connect
// over HTTP/2: TLS 1.3 in production, cleartext HTTP/2 on loopback for
// tests. New is the caller and Serve is the host process; both are built
// from the same generated code, so the daemon cannot drift into a second
// API, and a caller in any language can be generated from the schema.
//
// A client keeps two connections to a host. Control, terminals, journal
// metadata and the process log share one; projection and observation
// content travel on the other, so a large upload never queues behind or in
// front of a keystroke. Flow control is HTTP/2's own, per stream: a
// terminal whose output nobody reads fills its own window and stalls only
// itself, while the host keeps reading the shell into its history.
//
// A client says Hello first and names the features it needs. A host that
// lacks one refuses at once with ErrFeature. Versions are checked by
// feature, not by release stamp.
//
// Every request carries a bearer token, or the connection a verified client
// certificate. Each error carries the name of its execenv sentinel, so the
// Go client returns the same errors an in-process host would. Neither file
// bodies, PTY bytes nor tokens appear in an error or a log.
//
// An attached terminal lives as long as its output stream. Closing the
// Terminal, or losing the connection, ends the stream and detaches: the
// shell keeps running on the host for its grace, and a new connection may
// Attach again with Resume. Keystrokes travel in order on one input stream
// per attachment, and Close waits until the host has written them.
package remote
