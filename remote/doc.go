// Package remote implements the execenv Host contract over a single
// multiplexed connection.
//
// New is the caller. Serve is the host process. They share one frame
// protocol so the daemon cannot invent a second API. Closing a Terminal or
// losing the connection detaches it: the shell keeps running on the host,
// and a new connection may Attach again with Resume. Per-stream identities
// prevent late PTY frames from affecting a replacement stream, and each
// output frame names the absolute terminal offset of its first byte.
//
// Every terminal stream has its own credit-based flow control in both
// directions, with a 256 KiB window per direction granted in the attach
// exchange. The receiver returns credit as its caller consumes, in
// batches of at least half the window. A client therefore buffers at most
// one window of unread output per Terminal. While a reader is stalled the
// host stops reading that terminal; the session's history keeps absorbing
// shell output, and once the reader resumes a cursor that fell behind the
// history skips ahead, which Offset shows. Write blocks while the host has
// granted no input credit. PTY input does not count against the
// connection's operation limit. A stalled stream never delays RPCs or
// other streams, and a peer that sends past its credit, or grants credit
// it was never owed, fails only that stream with ErrInvalid. File bodies
// and PTY octets travel only in Extra, never in Status.
package remote
