# Changelog

## Unreleased

### Changed

- `Env.Observe` takes an `ObserveRequest` (`Fence`, `After`, `Limit`,
  `Wait`) in place of its positional arguments. With a `Wait` (at most
  `MaxObserveWait`) it holds the call until the guest has produced an
  observation after `After` or the wait has passed, and returns an empty
  page after the wait. Every host implements it: the memory host, the
  isolated host and the remote client and server, where the wait is added
  to both sides' operation timeout. `ValidateObserveRequest` checks one.
  The remote host offers it as the `observe_wait` feature, which the client
  requires. The guest link never waits: the isolated host looks again
  itself, so a freeze never queues behind a wait.
- `ExecutionObservation.ProcessTag` names the process started with
  `Processes.Start` that the change's writer descends from; a path several
  programs changed before one capture names the last. It is empty when the
  host cannot say. The guest agent maps the process behind each filesystem
  event to the tagged process it descends from through the guest kernel's
  process events; the in-memory host names it for what its shell's new
  `> path` and `>> path` redirections write. The remote host offers it as
  the `observation_tag` feature, which the client requires.
- `ProjectionMutation.Overwrite` lets a create, write or delete replace
  what the guest left at its path, unconfirmed changes included, instead
  of failing with `ErrConflict`. Every retained observation whose paths
  all lie in the replaced subtree is discarded: `ProjectionReceipt.Discarded`
  lists their host sequences, and `ExecutionObservation.DiscardedBy` names
  the overwriting journal cursor on later pages. A discarded observation
  cannot be confirmed and its content is `ErrStale`. An overwrite that
  would only half replace a guest move, or whose parent the guest removed,
  is still `ErrConflict`; a move or rename cannot overwrite
  (`ErrInvalid`). `ProjectionReceipt` is no longer comparable with `==`.
  The remote host offers it as the `projection_overwrite` feature, which
  the client requires.
- `execenvtest.Observed` has an optional `Processes`; `RunObservations`
  uses it to check `ProcessTag`, and covers waits and overwrites for every
  host. `execenvtest.Workspace` now includes `ConfirmObservationProjection`
  and `AcknowledgeObservations`. `execenvtest.FilesGuest` is a `Guest` over
  the file API, for hosts whose workspace a test cannot reach.
- `Observe` and `ObserveProcesses` are no longer activity for the idle
  timeout, so a caller that keeps an observation wait open does not keep
  an idle grant awake.
