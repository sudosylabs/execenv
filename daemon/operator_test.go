package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/memory"
	"github.com/sudosylabs/execenv/remote"
)

// shortSocket returns a Unix socket path short enough for every platform.
func shortSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "eo")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "op.sock")
}

func TestDrainStateMachine(t *testing.T) {
	t.Parallel()
	host, err := memory.New(memory.Config{Images: []execenv.Image{"default"}, Slots: 2})
	if err != nil {
		t.Fatal(err)
	}
	d, err := newDrain(host)
	if err != nil {
		t.Fatal(err)
	}
	step := func(op OperatorOp, state DrainState, grants int) {
		t.Helper()
		reply, err := d.do(t.Context(), op)
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		if reply.State != state || reply.Grants != grants {
			t.Fatalf("%s = %+v, want %s with %d grants", op, reply, state, grants)
		}
	}
	step(OperatorStatus, DrainServing, 0)
	step(OperatorDrain, DrainDrained, 0)
	step(OperatorUndrain, DrainServing, 0)
	held, err := host.Ensure(t.Context(), execenv.Spec{ID: "held", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	step(OperatorDrain, DrainDraining, 1)
	step(OperatorDrain, DrainDraining, 1)
	if _, err := host.Ensure(t.Context(), execenv.Spec{ID: "new", Image: "default"}); !errors.Is(err, execenv.ErrDraining) {
		t.Fatalf("Ensure() while draining error = %v, want ErrDraining", err)
	}
	if _, err := host.Ensure(t.Context(), execenv.Spec{ID: "held", Image: "default"}); err != nil {
		t.Fatalf("Ensure() of a held grant while draining error = %v", err)
	}
	report, err := host.Ready(t.Context())
	if err != nil || report.Usable || report.Reason != execenv.ReasonDraining {
		t.Fatalf("Ready() while draining = %+v %v", report, err)
	}
	if err := held.Revoke(t.Context()); err != nil {
		t.Fatal(err)
	}
	step(OperatorStatus, DrainDrained, 0)
	step(OperatorUndrain, DrainServing, 0)
	if _, err := host.Ensure(t.Context(), execenv.Spec{ID: "new", Image: "default"}); err != nil {
		t.Fatalf("Ensure() after undrain error = %v", err)
	}
	if _, err := d.do(t.Context(), "reboot"); !errors.Is(err, execenv.ErrInvalid) {
		t.Fatalf("unknown op error = %v, want ErrInvalid", err)
	}
}

// A lost grant does not hold a drain open.
func TestDrainCompletesWhenTheLastGrantIsLost(t *testing.T) {
	t.Parallel()
	host, err := memory.New(memory.Config{Images: []execenv.Image{"default"}, Slots: 1})
	if err != nil {
		t.Fatal(err)
	}
	d, err := newDrain(host)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.Ensure(t.Context(), execenv.Spec{ID: "held", Image: "default"}); err != nil {
		t.Fatal(err)
	}
	if reply, err := d.do(t.Context(), OperatorDrain); err != nil || reply.State != DrainDraining {
		t.Fatalf("drain = %+v %v", reply, err)
	}
	host.Lose("held")
	if reply, err := d.do(t.Context(), OperatorStatus); err != nil || reply.State != DrainDrained || reply.Grants != 0 {
		t.Fatalf("status after loss = %+v %v", reply, err)
	}
}

func TestOperatorSocketRoundTrip(t *testing.T) {
	t.Parallel()
	host, err := memory.New(memory.Config{Images: []execenv.Image{"default"}, Slots: 1})
	if err != nil {
		t.Fatal(err)
	}
	path := shortSocket(t)
	ln, err := listenOperator(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("operator socket mode = %v %v, want 0600", info, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = ServeOperator(ctx, ln, host) }()
	if _, err := host.Ensure(t.Context(), execenv.Spec{ID: "held", Image: "default"}); err != nil {
		t.Fatal(err)
	}
	reply, err := Operate(t.Context(), path, OperatorDrain)
	if err != nil || reply.State != DrainDraining || reply.Grants != 1 {
		t.Fatalf("Operate(drain) = %+v %v", reply, err)
	}
	if _, err := Operate(t.Context(), path, "reboot"); err == nil {
		t.Fatal("Operate(unknown) succeeded")
	}
	reply, err = Operate(t.Context(), path, OperatorUndrain)
	if err != nil || reply.State != DrainServing {
		t.Fatalf("Operate(undrain) = %+v %v", reply, err)
	}
}

func TestOperateWithoutDaemon(t *testing.T) {
	t.Parallel()
	if _, err := Operate(t.Context(), shortSocket(t), OperatorStatus); !errors.Is(err, ErrNoOperator) {
		t.Fatalf("Operate() error = %v, want ErrNoOperator", err)
	}
}

func TestListenOperatorReplacesOnlyStaleSockets(t *testing.T) {
	t.Parallel()
	path := shortSocket(t)
	ln, err := listenOperator(path)
	if err != nil {
		t.Fatal(err)
	}
	// A crashed daemon leaves its socket behind; the next one rebinds.
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = ln.Close()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("stale socket missing: %v", err)
	}
	again, err := listenOperator(path)
	if err != nil {
		t.Fatalf("rebind over a stale socket: %v", err)
	}
	_ = again.Close()
	plain := filepath.Join(filepath.Dir(path), "plain")
	if err := os.WriteFile(plain, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := listenOperator(plain); !errors.Is(err, execenv.ErrInvalid) {
		t.Fatalf("listenOperator() over a regular file error = %v, want ErrInvalid", err)
	}
	if raw, err := os.ReadFile(plain); err != nil || string(raw) != "keep" {
		t.Fatal("listenOperator() replaced a regular file")
	}
}

// The configured daemon serves the operator socket beside the grant wire,
// and a drain shows on the wire as ErrDraining and an unusable Ready.
func TestRunServesOperatorSocket(t *testing.T) {
	path := shortSocket(t)
	cfg, err := Load(writeConfig(t, `{
		"listen": "127.0.0.1:0",
		"token": "secret",
		"security": "insecure_local",
		"adapter": "memory",
		"images": [{"id": "default"}],
		"slots": 2,
		"operator_socket": "`+path+`"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	addr, err := listenAndRun(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	client, err := remote.New(remote.Config{Address: addr, Security: remote.SecurityInsecureLocal, Token: []byte("secret")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if _, err := client.Ensure(t.Context(), execenv.Spec{ID: "held", Image: "default"}); err != nil {
		t.Fatal(err)
	}
	var reply OperatorReply
	for range 100 {
		if reply, err = Operate(t.Context(), path, OperatorDrain); !errors.Is(err, ErrNoOperator) {
			break
		}
	}
	if err != nil || reply.State != DrainDraining || reply.Grants != 1 {
		t.Fatalf("Operate(drain) = %+v %v", reply, err)
	}
	report, err := client.Ready(t.Context())
	if err != nil || report.Usable || report.Reason != execenv.ReasonDraining || report.Grants != 1 {
		t.Fatalf("Ready() while draining = %+v %v", report, err)
	}
	if _, err := client.Ensure(t.Context(), execenv.Spec{ID: "new", Image: "default"}); !errors.Is(err, execenv.ErrDraining) {
		t.Fatalf("Ensure() while draining error = %v, want ErrDraining", err)
	}
	if err := client.Revoke(t.Context(), "held"); err != nil {
		t.Fatal(err)
	}
	if reply, err = Operate(t.Context(), path, OperatorStatus); err != nil || reply.State != DrainDrained {
		t.Fatalf("Operate(status) = %+v %v", reply, err)
	}
}

func TestLoadRejectsRelativeOperatorSocket(t *testing.T) {
	t.Parallel()
	_, err := Load(writeConfig(t, `{
		"listen": "127.0.0.1:0",
		"token": "secret",
		"security": "insecure_local",
		"adapter": "memory",
		"images": [{"id": "default"}],
		"slots": 1,
		"operator_socket": "operator.sock"
	}`))
	if !errors.Is(err, execenv.ErrInvalid) {
		t.Fatalf("Load() error = %v, want ErrInvalid", err)
	}
}
