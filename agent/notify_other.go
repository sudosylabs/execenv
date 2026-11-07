//go:build !linux

package agent

import "time"

// devPoll is the rescan interval on platforms without inotify. Guests are
// Linux; this exists for local development and tests of the agent itself.
const devPoll = 50 * time.Millisecond

func newNotifier() notifier { return newTickNotifier(devPoll) }
