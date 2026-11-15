package execenvtest

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
)

func runGrants(t *testing.T, factory Factory) {
	t.Helper()

	t.Run("grants are listed and followed through freeze and revoke", func(t *testing.T) {
		host := factory(t, Config{})
		env, fence := occupyRunning(t, host, "followed")
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		listed, err := host.ListGrants(ctx)
		if err != nil || len(listed) != 1 || listed[0].ID != "followed" || listed[0].Epoch != env.Epoch() || listed[0].State != execenv.GrantRunning || listed[0].Image != "default" {
			t.Fatalf("ListGrants() = %+v, %v", listed, err)
		}
		if listed[0].Started.IsZero() || listed[0].Resources.MemoryBytes == 0 {
			t.Fatalf("listed grant lacks its details: %+v", listed[0])
		}
		if g, err := host.WaitGrant(ctx, "followed"); err != nil || g.State != execenv.GrantRunning {
			t.Fatalf("WaitGrant() = %+v, %v", g, err)
		}
		w, err := host.WatchGrant(ctx, "followed")
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()
		if g, err := w.Next(ctx); err != nil || g.State != execenv.GrantRunning {
			t.Fatalf("first watched state = %+v, %v", g, err)
		}
		frozen := fence
		frozen.FenceRevision++
		if _, err := env.Control(ctx, execenv.ControlRequest{Fence: frozen, State: execenv.ControlFrozen}); err != nil {
			t.Fatal(err)
		}
		if g, err := w.Next(ctx); err != nil || g.State != execenv.GrantFrozen || g.FenceRevision != frozen.FenceRevision {
			t.Fatalf("watched freeze = %+v, %v", g, err)
		}
		if err := host.Revoke(ctx, "followed"); err != nil {
			t.Fatal(err)
		}
		if g, err := w.Next(ctx); err != nil || g.State != execenv.GrantRevoked {
			t.Fatalf("watched revoke = %+v, %v", g, err)
		}
		if _, err := w.Next(ctx); !errors.Is(err, io.EOF) {
			t.Fatalf("watch after revoke = %v, want io.EOF", err)
		}
		if listed, err := host.ListGrants(ctx); err != nil || len(listed) != 0 {
			t.Fatalf("ListGrants() after revoke = %+v, %v", listed, err)
		}
	})

	t.Run("an unknown grant is not found", func(t *testing.T) {
		host := factory(t, Config{})
		if _, err := host.WaitGrant(t.Context(), "nobody"); !errors.Is(err, execenv.ErrNotFound) {
			t.Fatalf("WaitGrant() = %v, want ErrNotFound", err)
		}
		if _, err := host.WatchGrant(t.Context(), "nobody"); !errors.Is(err, execenv.ErrNotFound) {
			t.Fatalf("WatchGrant() = %v, want ErrNotFound", err)
		}
	})

	t.Run("the host watch reports grants and capacity", func(t *testing.T) {
		host := factory(t, Config{})
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		w, err := host.WatchHost(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()
		ev, err := w.Next(ctx)
		if err != nil || ev.Capacity == nil || ev.Capacity.MemoryBytes.Reserved != 0 {
			t.Fatalf("first host event = %+v, %v", ev, err)
		}
		env, _ := occupyRunning(t, host, "watched")
		var sawGrant, sawCapacity bool
		for !sawGrant || !sawCapacity {
			ev, err := w.Next(ctx)
			if err != nil {
				t.Fatalf("host watch = %v (grant %v, capacity %v)", err, sawGrant, sawCapacity)
			}
			if ev.Grant != nil && ev.Grant.ID == "watched" && ev.Grant.State == execenv.GrantRunning && ev.Grant.Epoch == env.Epoch() {
				sawGrant = true
			}
			if ev.Capacity != nil && ev.Capacity.MemoryBytes.Reserved > 0 {
				sawCapacity = true
			}
		}
	})
}
