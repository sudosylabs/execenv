// Package execenv occupies isolated execution grants on one host.
//
// Callers ensure a grant, project a POSIX tree, attach one PTY, freeze and
// thaw I/O, and revoke. Isolation machinery stays behind the contract.
//
// The projected tree is not durable authority. ReplaceTree and Apply push
// caller state into the grant. Watch and Open report what the isolated
// environment wrote. Watch cursors permit bounded replay after a connection
// loss; ReplaceTree starts a new observation generation. Losing a grant must
// not be treated as losing caller state the caller already has.
package execenv
