package portconn

import (
	"context"
	"errors"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sudosylabs/execenv"
)

func TestDialDistinguishesGuestTimeoutFromCallerDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		_, err := dialLoopback(t.Context(), 1234, true, true, time.Second, dial)
		if !errors.Is(err, execenv.ErrConnection) {
			t.Fatalf("guest dial timed out with %v", err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		_, err = dialLoopback(ctx, 1234, true, true, 2*time.Second, dial)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("caller deadline returned %v", err)
		}
	})
}
