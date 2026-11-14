// Package execenv occupies isolated execution grants on one host.
//
// Callers ensure a grant, project a POSIX tree, attach to its terminal,
// control execution under a fence, and revoke. Isolation machinery stays
// behind the contract.
//
// A grant has at most one terminal session: one shell and the most recent
// TerminalHistoryBytes of its output, counted in absolute offsets from the
// shell's first byte. Clients attach to the session; at most one is
// attached at a time, and Takeover replaces it. Closing a Terminal
// detaches: the shell keeps running for the host's terminal grace, and a
// later Attach resumes it from a TerminalPosition. Hangup, grace expiry,
// shell exit and revoke end the session.
//
// The projected tree is not durable authority. The caller owns a journal;
// InitializeProjection and ApplyProjection push consecutive journal entries
// into the grant, each bound to a ProjectionFence. Observe reports what the
// isolated environment wrote, with immutable captured content, until the
// caller acknowledges it. IgnoreRules name a caller-chosen file of
// gitignore patterns for paths Observe leaves out; held paths stay
// observed. Losing a grant must not be treated as losing
// caller state the caller already has.
//
// ObserveProcesses is the environment's process log: every program the
// contained workload starts and how each process ends, in order, under the
// same fence. The guest helper records it outside the workload and sends
// each event as soon as it is taken, so the workload cannot see, alter or
// suppress an event already recorded. The host retains events until the
// caller acknowledges them; a full log counts what it cannot keep and
// reports the count as one ProcessDropped event, never silently.
package execenv
