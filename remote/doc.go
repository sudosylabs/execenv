// Package remote implements the execenv Host contract over a single
// multiplexed connection.
//
// New is the caller. Serve is the host process. They share one frame
// protocol so the daemon cannot invent a second API. Closing a Terminal or
// losing the connection detaches it: the shell keeps running on the host,
// and a new connection may Attach again with Resume. Per-stream identities
// prevent late PTY frames from affecting a replacement stream, and each
// output frame names the absolute terminal offset of its first byte. A
// client buffers at most 1 MiB of unread output per Terminal; past that the
// Terminal fails with ErrLagged and detaches, and the caller resumes from
// its Offset. File bodies and PTY octets travel only in Extra, never in
// Status.
package remote
