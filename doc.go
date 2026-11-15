// Package execenv occupies isolated execution grants on one host.
//
// Callers ensure a grant, project a POSIX tree or work on its files, run
// processes in it, control execution under a fence, and revoke. Isolation
// machinery stays behind the contract.
//
// A grant runs processes by tag: a shell is a process with a terminal, a
// command one without. Each keeps the most recent output of each stream,
// counted in absolute offsets from its first byte, so any number of readers
// attach and resume where they left off. Input is taken by offset, so a
// resend after a lost connection duplicates nothing. A process ends on its
// own, on a signal, on its timeout, or once left unattached past its grace.
//
// The projected tree is not durable authority. The caller owns a journal;
// InitializeProjection and ApplyProjection push consecutive journal entries
// into the grant, each bound to a ProjectionFence. Observe reports what the
// isolated environment wrote, with immutable captured content, until the
// caller acknowledges it; with a Wait it holds the call until there is
// something new, so a caller never polls. A projection mutation marked
// Overwrite replaces the guest's unconfirmed change on its path instead of
// refusing, and the receipt lists the observations it discarded.
// IgnoreRules name a caller-chosen file of
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
//
// A grant's environment can end without a revoke, when its machine stops
// or its guest crashes. The host notices on its own, frees every resource the grant held, and fails every call on that occupancy
// with ErrLost. Ensure of the same id fails with ErrLost too, until the
// caller acknowledges the loss with Revoke; the next Ensure then occupies
// the id under a new Epoch, so no fence of the lost epoch can apply to it.
//
// A host's operator can drain it before a restart. A draining host serves
// the grants it holds, refuses new ones with ErrDraining, and reports
// Usable false with ReasonDraining in Ready.
package execenv
