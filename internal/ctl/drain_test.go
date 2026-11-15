package ctl

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/daemon"
	"github.com/sudosylabs/execenv/memory"
)

func init() { drainPoll = 10 * time.Millisecond }

func TestRestartVerdict(t *testing.T) {
	t.Parallel()
	unreachable := daemon.ErrNoOperator
	cases := []struct {
		name   string
		active bool
		reply  daemon.OperatorReply
		err    error
		ok     bool
	}{
		{"serving without grants", true, daemon.OperatorReply{State: daemon.DrainServing}, nil, true},
		{"drained", true, daemon.OperatorReply{State: daemon.DrainDrained}, nil, true},
		{"serving with grants", true, daemon.OperatorReply{State: daemon.DrainServing, Grants: 2}, nil, false},
		{"draining with grants", true, daemon.OperatorReply{State: daemon.DrainDraining, Grants: 1}, nil, false},
		{"unreachable and stopped", false, daemon.OperatorReply{}, unreachable, true},
		{"unreachable but running", true, daemon.OperatorReply{}, unreachable, false},
		{"durable grants survive", true, daemon.OperatorReply{State: daemon.DrainServing, Grants: 3, Durable: true}, nil, true},
	}
	for _, c := range cases {
		err := restartVerdict(c.active, c.reply, c.err)
		if (err == nil) != c.ok {
			t.Errorf("%s: restartVerdict() = %v, want ok=%v", c.name, err, c.ok)
		}
		if c.reply.Grants > 0 && !c.reply.Durable && !errors.Is(err, errGrantsLive) {
			t.Errorf("%s: error = %v, want errGrantsLive", c.name, err)
		}
	}
}

// liveDaemon serves the operator socket for a memory host and points the
// host config at it. The socket lives outside opts.State so its path stays
// short enough to bind.
func liveDaemon(t *testing.T, opts Options) *memory.Host {
	t.Helper()
	host, err := memory.New(memory.Config{Images: []execenv.Image{"default"}, Capacity: execenv.DefaultGrant.Times(4)})
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "ec")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "op.sock")
	doc, err := daemon.Load(opts.configPath())
	if err != nil {
		t.Fatal(err)
	}
	doc.OperatorSocket = path
	if err := daemon.Save(opts.configPath(), doc); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = daemon.ServeOperator(ctx, ln, host) }()
	return host
}

// A durable daemon restarts with grants live, but an image its grants run
// from is never removed, not even with --force.
func TestDurableDaemonRestartsButKeepsImagesInUse(t *testing.T) {
	t.Parallel()
	opts := bootstrappedWithFixture(t, t.TempDir())
	host := liveDaemon(t, opts)
	occupy(t, host, "held")
	reply, err := opts.operate(daemon.OperatorStatus)
	if err != nil || reply.Durable || len(reply.Images) != 1 || reply.Images[0] != "default" {
		t.Fatalf("status = %+v, %v", reply, err)
	}
	if err := restartVerdict(true, daemon.OperatorReply{Grants: 1, Durable: true}, nil); err != nil {
		t.Fatalf("durable restart refused: %v", err)
	}
	used := daemon.OperatorReply{Grants: 1, Durable: true, Images: []string{"fixture"}}
	if !imageInUse(used, "fixture") || imageInUse(used, "other") || imageInUse(daemon.OperatorReply{Images: []string{"fixture"}}, "fixture") {
		t.Fatal("imageInUse() misjudged")
	}
}

// newCountingServer is a release channel that serves nothing and counts
// every request it gets.
func newCountingServer(t *testing.T, hits *atomic.Int64) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func occupy(t *testing.T, host *memory.Host, id execenv.ID) execenv.Env {
	t.Helper()
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: id, Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func TestRemoveRefusesWhileGrantsLive(t *testing.T) {
	t.Parallel()
	opts := bootstrappedWithFixture(t, t.TempDir())
	host := liveDaemon(t, opts)
	occupy(t, host, "held")
	reloads := 0
	opts.Reload = func() error { reloads++; return nil }
	before, err := daemon.Load(opts.configPath())
	if err != nil {
		t.Fatal(err)
	}
	err = Remove(opts, "fixture", io.Discard)
	if !errors.Is(err, errGrantsLive) || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("Remove() error = %v, want a refusal naming --force", err)
	}
	after, err := daemon.Load(opts.configPath())
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Images) != 1 || reloads != 0 {
		t.Fatalf("refused remove changed the host: images=%d reloads=%d", len(after.Images), reloads)
	}
	if _, err := os.Stat(before.Images[0].Rootfs); err != nil {
		t.Fatalf("refused remove deleted the disk: %v", err)
	}
	opts.Force = true
	if err := Remove(opts, "fixture", io.Discard); err != nil {
		t.Fatalf("Remove(--force) error = %v", err)
	}
	if reloads != 1 {
		t.Fatalf("reloads = %d, want 1", reloads)
	}
}

func TestInstallRefusesBeforeFetching(t *testing.T) {
	t.Parallel()
	opts := bootstrappedWithFixture(t, t.TempDir())
	host := liveDaemon(t, opts)
	occupy(t, host, "held")
	var hits atomic.Int64
	opts.ReleaseURL = newCountingServer(t, &hits)
	if err := Install(opts, "fixture", io.Discard); !errors.Is(err, errGrantsLive) {
		t.Fatalf("Install() error = %v, want errGrantsLive", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("refused install fetched %d files", n)
	}
}

func TestUpgradeRefusesBeforeReplacingBinaries(t *testing.T) {
	t.Parallel()
	opts := bootstrappedWithFixture(t, t.TempDir())
	host := liveDaemon(t, opts)
	occupy(t, host, "held")
	var hits atomic.Int64
	opts.ReleaseURL = newCountingServer(t, &hits)
	if err := Upgrade(opts, io.Discard); !errors.Is(err, errGrantsLive) {
		t.Fatalf("Upgrade() error = %v, want errGrantsLive", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("refused upgrade fetched %d files", n)
	}
}

func TestRestartRefusedWhenRunningDaemonDoesNotAnswer(t *testing.T) {
	t.Parallel()
	opts := bootstrappedWithFixture(t, t.TempDir())
	opts.UnitActive = func() bool { return true }
	reloads := 0
	opts.Reload = func() error { reloads++; return nil }
	if err := Remove(opts, "fixture", io.Discard); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("Remove() error = %v, want a refusal naming --force", err)
	}
	if reloads != 0 {
		t.Fatalf("reloads = %d", reloads)
	}
}

// Drain waits for the last grant, after which a restart needs no --force.
func TestDrainWaitsThenRestartProceeds(t *testing.T) {
	t.Parallel()
	opts := bootstrappedWithFixture(t, t.TempDir())
	host := liveDaemon(t, opts)
	first, second := occupy(t, host, "first"), occupy(t, host, "second")
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = first.Revoke(context.Background())
		time.Sleep(50 * time.Millisecond)
		_ = second.Revoke(context.Background())
	}()
	var out bytes.Buffer
	if err := Drain(opts, 5*time.Second, &out); err != nil {
		t.Fatalf("Drain() error = %v", err)
	}
	for _, want := range []string{"state=draining grants=2", "state=draining grants=1", "state=drained grants=0"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("Drain() output %q lacks %q", out.String(), want)
		}
	}
	if _, err := host.Ensure(t.Context(), execenv.Spec{ID: "late", Image: "default"}); !errors.Is(err, execenv.ErrDraining) {
		t.Fatalf("Ensure() after drain error = %v, want ErrDraining", err)
	}
	reloads := 0
	opts.Reload = func() error { reloads++; return nil }
	if err := Remove(opts, "fixture", io.Discard); err != nil {
		t.Fatalf("Remove() of a drained host error = %v", err)
	}
	if reloads != 1 {
		t.Fatalf("reloads = %d, want 1", reloads)
	}
}

func TestDrainTimesOutAndStaysDraining(t *testing.T) {
	t.Parallel()
	opts := bootstrappedWithFixture(t, t.TempDir())
	host := liveDaemon(t, opts)
	occupy(t, host, "held")
	var out bytes.Buffer
	err := Drain(opts, 50*time.Millisecond, &out)
	if err == nil || !strings.Contains(err.Error(), "1 grants remaining") {
		t.Fatalf("Drain() error = %v, want a timeout naming the remaining grants", err)
	}
	if !strings.Contains(out.String(), "state=draining grants=1") {
		t.Fatalf("Drain() output = %q", out.String())
	}
	report, err := host.Ready(t.Context())
	if err != nil || report.Usable || report.Reason != execenv.ReasonDraining {
		t.Fatalf("Ready() after a timed-out drain = %+v %v", report, err)
	}
	if err := Undrain(opts, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "state=serving grants=1") {
		t.Fatalf("Undrain() output = %q", out.String())
	}
	if _, err := host.Ensure(t.Context(), execenv.Spec{ID: "new", Image: "default"}); err != nil {
		t.Fatalf("Ensure() after undrain error = %v", err)
	}
}

func TestDrainWithZeroTimeoutReturnsAtOnce(t *testing.T) {
	t.Parallel()
	opts := bootstrappedWithFixture(t, t.TempDir())
	host := liveDaemon(t, opts)
	occupy(t, host, "held")
	var out bytes.Buffer
	if err := Drain(opts, 0, &out); err != nil {
		t.Fatalf("Drain(0) error = %v", err)
	}
	if !strings.Contains(out.String(), "state=draining grants=1") {
		t.Fatalf("Drain(0) output = %q", out.String())
	}
}

func TestDrainNeedsADaemon(t *testing.T) {
	t.Parallel()
	opts := bootstrappedWithFixture(t, t.TempDir())
	if err := Drain(opts, time.Second, io.Discard); !errors.Is(err, daemon.ErrNoOperator) {
		t.Fatalf("Drain() without a daemon error = %v, want ErrNoOperator", err)
	}
}

func TestStatusReportsDrainState(t *testing.T) {
	t.Parallel()
	opts := bootstrappedWithFixture(t, t.TempDir())
	host := liveDaemon(t, opts)
	occupy(t, host, "held")
	var out bytes.Buffer
	if err := Status(opts, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "drain=serving\ngrants=1\n") {
		t.Fatalf("Status() output = %q", out.String())
	}
}

func TestBootstrapNamesTheOperatorSocket(t *testing.T) {
	t.Parallel()
	opts := testOpts(t, t.TempDir())
	if err := Bootstrap(opts, io.Discard); err != nil {
		t.Fatal(err)
	}
	doc, err := daemon.Load(opts.configPath())
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(opts.State, "operator.sock"); doc.OperatorSocket != want {
		t.Fatalf("operator_socket = %q, want %q", doc.OperatorSocket, want)
	}
}
