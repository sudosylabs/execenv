package agent

import "time"

// notifier tells the watcher that something under a registered directory
// may have changed. It carries no paths: the watcher always rescans, so a
// notifier may coalesce, drop, or overflow without losing an event.
type notifier interface {
	// Events receives at least one value after any change under a
	// registered directory. Values coalesce.
	Events() <-chan struct{}
	// Add registers a directory. Adding the same directory twice is a no-op.
	Add(dir string)
	Close()
}

// tickNotifier signals on a fixed interval. It is the fallback when the
// platform has no filesystem notifications or registration failed.
type tickNotifier struct {
	ch   chan struct{}
	stop chan struct{}
}

func newTickNotifier(every time.Duration) *tickNotifier {
	n := &tickNotifier{ch: make(chan struct{}, 1), stop: make(chan struct{})}
	go func() {
		tick := time.NewTicker(every)
		defer tick.Stop()
		for {
			select {
			case <-n.stop:
				return
			case <-tick.C:
				select {
				case n.ch <- struct{}{}:
				default:
				}
			}
		}
	}()
	return n
}

func (n *tickNotifier) Events() <-chan struct{} { return n.ch }
func (n *tickNotifier) Add(string)              {}
func (n *tickNotifier) Close()                  { close(n.stop) }
