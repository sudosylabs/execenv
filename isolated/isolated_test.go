package isolated

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/execenvtest"
)

func TestReadyFailsClosedWhenProbeFails(t *testing.T) {
	t.Parallel()
	host := testHost(t, func() error { return execenv.ErrUnavailable }, &recordingLauncher{})
	if !host.Capabilities().Isolated {
		t.Fatal("Capabilities.Isolated = false")
	}
	report, err := host.Ready(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.Usable {
		t.Fatal("Ready() Usable = true")
	}
	_, err = host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if !errors.Is(err, execenv.ErrUnavailable) {
		t.Fatalf("Ensure() error = %v, want ErrUnavailable", err)
	}
}

func TestNewCleansStaleGrantsAndLocksWorkDir(t *testing.T) {
	workDir := t.TempDir()
	stale := filepath.Join(workDir, "grants", "stale", "workspace", "left.txt")
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	imageDir := t.TempDir()
	image := writeCatalogImage(t, imageDir, "default", "root")
	host, err := New(Config{WorkDir: workDir, Images: []Image{image}, Capacity: execenv.DefaultGrant.Times(1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.lock.Close() })
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale grant stat error = %v, want not exist", err)
	}
	if _, err := New(Config{WorkDir: workDir, Images: []Image{image}, Capacity: execenv.DefaultGrant.Times(1)}); err == nil {
		t.Fatal("second host acquired the same work directory")
	}
}

func TestReadyFailsClosedWhenDeviceMissing(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("device probe is linux-only")
	}
	dir := t.TempDir()
	host, err := New(Config{
		WorkDir:  t.TempDir(),
		Capacity: execenv.DefaultGrant.Times(1),
		Device:   filepath.Join(dir, "missing"),
		Images:   []Image{writeCatalogImage(t, dir, "default", "rootfs")},
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := host.Ready(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.Usable {
		t.Fatal("Ready() Usable = true without an isolation device")
	}
	_, err = host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if !errors.Is(err, execenv.ErrUnavailable) {
		t.Fatalf("Ensure() error = %v, want ErrUnavailable", err)
	}
}

func TestEnsureStartsOneMachineAndReattaches(t *testing.T) {
	t.Parallel()
	launch := &recordingLauncher{}
	host := testHost(t, func() error { return nil }, launch)
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if env.ID() != again.ID() {
		t.Fatal("reattach returned a different grant")
	}
	launch.mu.Lock()
	starts := launch.starts
	launch.mu.Unlock()
	if starts != 1 {
		t.Fatalf("starts = %d, want 1", starts)
	}
}

func TestConcurrentEnsureCoalescesOneLaunch(t *testing.T) {
	launch := &blockingLauncher{started: make(chan struct{}), release: make(chan struct{})}
	host := testHost(t, func() error { return nil }, launch)
	type result struct {
		env execenv.Env
		err error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			env, err := host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
			results <- result{env: env, err: err}
		}()
	}
	<-launch.started
	close(launch.release)
	first := <-results
	second := <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("Ensure errors = %v, %v", first.err, second.err)
	}
	if first.env.ID() != second.env.ID() {
		t.Fatalf("Ensure IDs = %q, %q", first.env.ID(), second.env.ID())
	}
	launch.inner.mu.Lock()
	starts := launch.inner.starts
	launch.inner.mu.Unlock()
	if starts != 1 {
		t.Fatalf("starts = %d, want 1", starts)
	}
}

func TestEnsureConflictsOnImageChange(t *testing.T) {
	t.Parallel()
	host := testHost(t, func() error { return nil }, &recordingLauncher{})
	if _, err := host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"}); err != nil {
		t.Fatal(err)
	}
	_, err := host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "alt"})
	if !errors.Is(err, execenv.ErrConflict) {
		t.Fatalf("Ensure() error = %v, want ErrConflict", err)
	}
}

// runningFence enters the fenced protocol at revision one.
func runningFence(t *testing.T, env execenv.Env) execenv.ProjectionFence {
	t.Helper()
	fence := execenv.ProjectionFence{ExecutionGrantID: env.ID(), EnvironmentEpoch: env.Epoch(), FenceRevision: 1}
	if _, err := env.Control(t.Context(), execenv.ControlRequest{Fence: fence, State: execenv.ControlRunning}); err != nil {
		t.Fatal(err)
	}
	return fence
}

func TestFrozenControlPausesAndRevokeStops(t *testing.T) {
	t.Parallel()
	launch := &recordingLauncher{}
	host := testHost(t, func() error { return nil }, launch)
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	fence := runningFence(t, env)
	fence.FenceRevision++
	if _, err := env.Control(t.Context(), execenv.ControlRequest{Fence: fence, State: execenv.ControlFrozen}); err != nil {
		t.Fatal(err)
	}
	launch.mu.Lock()
	inst := launch.instances[0]
	launch.mu.Unlock()
	inst.mu.Lock()
	paused, stopped := inst.paused, inst.stopped
	inst.mu.Unlock()
	if !paused || stopped {
		t.Fatalf("after freeze paused=%v stopped=%v", paused, stopped)
	}
	if _, err := env.UploadProjectionContent(t.Context(), fence, strings.NewReader("x")); !errors.Is(err, execenv.ErrFrozen) {
		t.Fatalf("upload while frozen error = %v", err)
	}
	fence.FenceRevision++
	if _, err := env.Control(t.Context(), execenv.ControlRequest{Fence: fence, State: execenv.ControlRunning}); err != nil {
		t.Fatal(err)
	}
	if err := env.Revoke(t.Context()); err != nil {
		t.Fatal(err)
	}
	inst.mu.Lock()
	stopped = inst.stopped
	inst.mu.Unlock()
	if !stopped {
		t.Fatal("revoke did not stop the instance")
	}
	if err := host.Revoke(t.Context(), "grant-1"); err != nil {
		t.Fatal(err)
	}
}

func TestInitializeProjectionWritesWorkspaceMount(t *testing.T) {
	t.Parallel()
	host := testHost(t, func() error { return nil }, &recordingLauncher{})
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	fence := runningFence(t, env)
	content, err := env.UploadProjectionContent(t.Context(), fence, strings.NewReader("package main\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.InitializeProjection(t.Context(), execenv.ProjectionInitialization{Fence: fence, MutationID: "init", Entries: []execenv.ProjectionEntry{
		{EntryID: "src", Kind: execenv.ProjectionDirectory, Path: "src"},
		{EntryID: "main", Kind: execenv.ProjectionFile, Path: "src/main.go", ContentVersion: "v1", Content: &content},
	}}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(host.cfg.WorkDir, "grants", "grant-1", workspaceName, "src", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "package main\n" {
		t.Fatal("workspace mount did not receive the file")
	}
}

func TestConformance(t *testing.T) {
	execenvtest.Run(t, func(t *testing.T, cfg execenvtest.Config) execenv.Host {
		t.Helper()
		host := testHost(t, func() error { return nil }, &recordingLauncher{})
		host.cfg.TerminalGrace = cfg.TerminalGrace
		return host
	})
}

func TestTouchInPtyBecomesObservation(t *testing.T) {
	t.Parallel()
	host := testHost(t, func() error { return nil }, &recordingLauncher{})
	env, err := host.Ensure(t.Context(), execenv.Spec{ID: "grant-1", Image: "default"})
	if err != nil {
		t.Fatal(err)
	}
	fence := runningFence(t, env)
	if _, err := env.InitializeProjection(t.Context(), execenv.ProjectionInitialization{Fence: fence, MutationID: "init"}); err != nil {
		t.Fatal(err)
	}
	term, err := env.Attach(t.Context(), execenv.AttachRequest{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = term.Close() })
	if _, err := term.Write([]byte("touch seen.txt\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		page, err := env.Observe(t.Context(), fence, 0, 128)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range page.Observations {
			if event.Operation == execenv.ObservationCreateFile && event.Path == "seen.txt" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("touch was not observed")
}

func TestExportedIdentifiersOmitHypervisorNames(t *testing.T) {
	t.Parallel()
	banned := []string{"Firecracker", "firecracker", "jailer", "Jailer", "KVM", "kvm", "vsock"}
	roots := []string{
		filepath.Join("isolated.go"),
		filepath.Join("launch.go"),
		filepath.Join("env.go"),
		filepath.Join("control.go"),
		filepath.Join("projection.go"),
		filepath.Join("catalog.go"),
	}
	for _, name := range roots {
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		for _, word := range banned {
			if strings.Contains(text, word) {
				t.Fatalf("%s contains %q", name, word)
			}
		}
	}
}

func testHost(t *testing.T, probe func() error, launch launcher) *Host {
	t.Helper()
	dir := t.TempDir()
	return testHostWithImages(t, probe, launch,
		writeCatalogImage(t, dir, "default", "root-default"),
		writeCatalogImage(t, dir, "alt", "root-alt"),
	)
}

func testHostWithImages(t *testing.T, probe func() error, launch launcher, images ...Image) *Host {
	t.Helper()
	h, err := New(Config{
		WorkDir:  t.TempDir(),
		Capacity: execenv.DefaultGrant.Times(2),
		Images:   images,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.probe = probe
	h.launch = launch
	h.attach = &recordingAttacher{}
	t.Cleanup(func() {
		h.mu.Lock()
		ids := make([]execenv.ID, 0, len(h.grants))
		for id := range h.grants {
			ids = append(ids, id)
		}
		h.mu.Unlock()
		for _, id := range ids {
			_ = h.Revoke(context.Background(), id)
		}
		_ = h.lock.Close()
	})
	return h
}

func TestPlatformProbeFailsOnThisOS(t *testing.T) {
	t.Parallel()
	err := probePlatform(Config{})
	if runtime.GOOS != "linux" && !errors.Is(err, execenv.ErrUnavailable) {
		t.Fatalf("probePlatform() error = %v, want ErrUnavailable on %s", err, runtime.GOOS)
	}
}
