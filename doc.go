// Package execenv occupies isolated execution grants on one host.
//
// Callers ensure a grant, project a POSIX tree, attach one PTY, control
// execution under a fence, and revoke. Isolation machinery stays behind the
// contract.
//
// The projected tree is not durable authority. The caller owns a journal;
// InitializeProjection and ApplyProjection push consecutive journal entries
// into the grant, each bound to a ProjectionFence. Observe reports what the
// isolated environment wrote, with immutable captured content, until the
// caller acknowledges it. Losing a grant must not be treated as losing
// caller state the caller already has.
package execenv
