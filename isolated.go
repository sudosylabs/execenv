package execenv

// RequireIsolated fails closed when host is not an isolated adapter.
// Production composition uses this so an in-memory host cannot be selected.
// Callers that need a particular class read Report.Isolation and apply their
// own policy.
func RequireIsolated(host Host) error {
	if host == nil || !host.Capabilities().Isolated {
		return Error("isolate", ErrUnavailable)
	}
	return nil
}
