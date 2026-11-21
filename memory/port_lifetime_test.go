package memory_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/execenvtest"
	"github.com/sudosylabs/execenv/memory"
)

func TestPortIdleLifetime(t *testing.T) {
	const idle = 200 * time.Millisecond
	for _, hold := range []bool{false, true} {
		name := "list polling does not hold grant"
		if hold {
			name = "connection holds grant until closed"
		}
		t.Run(name, func(t *testing.T) {
			host, err := memory.New(memory.Config{Images: []execenv.Image{"default"}, Capacity: execenv.DefaultGrant})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
			defer cancel()
			env, err := host.Ensure(ctx, execenv.Spec{ID: "ports-idle", Image: "default", IdleTimeout: idle, IdleAction: execenv.IdleFreeze})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = env.Revoke(context.Background()) })
			fence := execenv.ProjectionFence{ExecutionGrantID: env.ID(), EnvironmentEpoch: env.Epoch(), FenceRevision: 1}
			if _, err := env.Control(ctx, execenv.ControlRequest{Fence: fence, State: execenv.ControlRunning}); err != nil {
				t.Fatal(err)
			}
			if hold {
				port, _ := execenvtest.LocalPortListener(host)(t, env, fence, "127.0.0.1", "hold")
				c, err := env.ConnectPort(ctx, fence, port)
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				for until := time.Now().Add(3 * idle); time.Now().Before(until); {
					if _, err := env.ListPorts(ctx); err != nil {
						t.Fatalf("grant froze with an open port: %v", err)
					}
					time.Sleep(10 * time.Millisecond)
				}
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
			}
			for ctx.Err() == nil {
				_, err := env.ListPorts(ctx)
				if errors.Is(err, execenv.ErrFrozen) {
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Fatal("grant did not freeze while ListPorts was polling")
		})
	}
}

func TestRegisteredPortListenerTag(t *testing.T) {
	host, err := memory.New(memory.Config{Images: []execenv.Image{"default"}, Capacity: execenv.DefaultGrant})
	if err != nil {
		t.Fatal(err)
	}
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: "tagged", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Revoke(context.Background()) })
	fence := execenv.ProjectionFence{ExecutionGrantID: env.ID(), EnvironmentEpoch: env.Epoch(), FenceRevision: 1}
	if _, err := env.Control(t.Context(), execenv.ControlRequest{Fence: fence, State: execenv.ControlRunning}); err != nil {
		t.Fatal(err)
	}
	l, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := host.RegisterPortListener(env, l, "server"); err != nil {
		t.Fatal(err)
	}
	ports, err := env.ListPorts(t.Context())
	if err != nil || len(ports) != 1 || ports[0].Tag != "server" {
		t.Fatalf("registered ports = %v, %v", ports, err)
	}
}
