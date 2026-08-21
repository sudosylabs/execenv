// Package remote implements the execenv Host contract over a single
// multiplexed connection.
//
// New is the caller. Serve is the host process. They share one frame
// protocol so the later isolation daemon cannot invent a second API.
// A dropped PTY is hangup: the grant stays, and the caller may Attach
// again. Per-stream identities prevent late PTY or Watch frames from affecting
// a replacement stream. File bodies and PTY octets travel only in Extra,
// never in Status.
package remote
