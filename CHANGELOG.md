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
- `Observe` and `ObserveProcesses` are no longer activity for the idle
  timeout, so a caller that keeps an observation wait open does not keep
  an idle grant awake.
