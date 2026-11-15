package execenvtest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/sudosylabs/execenv"
)

// Workspace is the projection and observation surface of one environment.
// Every execenv.Env satisfies it.
type Workspace interface {
	UploadProjectionContent(ctx context.Context, fence execenv.ProjectionFence, body io.Reader) (execenv.ContentReference, error)
	InitializeProjection(ctx context.Context, request execenv.ProjectionInitialization) (execenv.ProjectionReceipt, error)
	ApplyProjection(ctx context.Context, request execenv.ProjectionApply) (execenv.ProjectionReceipt, error)
	Observe(ctx context.Context, request execenv.ObserveRequest) (execenv.ObservationPage, error)
	ConfirmObservationProjection(ctx context.Context, binding execenv.ObservationProjection) error
	AcknowledgeObservations(ctx context.Context, fence execenv.ProjectionFence, through uint64) error
}

// Guest acts inside one environment's workspace as its own processes would.
// Parents must already exist.
type Guest interface {
	Write(ctx context.Context, path string, data []byte) error
	Mkdir(ctx context.Context, path string) error
	Move(ctx context.Context, from, to string) error
	Remove(ctx context.Context, path string) error
}

// Observed is one running environment whose projection is not yet
// initialized, the fence that drives it, and a guest acting inside it.
// Processes, when set, runs processes in the same environment, and the host
// names on each observation the started process its writer descends from.
type Observed struct {
	Workspace Workspace
	Fence     execenv.ProjectionFence
	Guest     Guest
	Processes execenv.Processes
}

// ObservedFactory returns a fresh Observed for one subtest.
type ObservedFactory func(t *testing.T) Observed

// RunIgnore exercises IgnoreRules against guest-originated changes. It needs
// a real or simulated guest, so it is separate from Run.
func RunIgnore(t *testing.T, factory ObservedFactory) {
	t.Helper()
	const rules = "observe.ignore"

	t.Run("patterns suppress new guest paths", func(t *testing.T) {
		w := start(t, factory, []seed{{path: rules, body: "*.log\nbuild/\n"}}, &execenv.IgnoreRules{File: rules})
		w.mkdir("build")
		w.write("build/out.bin", "x")
		w.write("debug.log", "x")
		w.write("keep.txt", "x")
		w.expect("create_file keep.txt")
	})

	t.Run("held paths stay observed when a pattern matches them", func(t *testing.T) {
		w := start(t, factory, []seed{{path: rules, body: "*.log\nbuild/\n"}, {path: "app.log", body: "a"}, {path: "build", dir: true}, {path: "build/held.txt", body: "h"}}, &execenv.IgnoreRules{File: rules})
		w.write("app.log", "changed")
		w.write("build/held.txt", "changed")
		w.write("build/new.txt", "unheld")
		w.expect("write_file app.log", "write_file build/held.txt")
		w.move("app.log", "renamed.log")
		w.expect("move app.log -> renamed.log")
		w.write("renamed.log", "again")
		w.expect("write_file renamed.log")
	})

	t.Run("negation re-includes a path", func(t *testing.T) {
		w := start(t, factory, []seed{{path: rules, body: "*.tmp\n!keep.tmp\nbuild/*\n!build/keep.txt\n"}}, &execenv.IgnoreRules{File: rules})
		w.write("drop.tmp", "x")
		w.write("keep.tmp", "x")
		w.mkdir("build")
		w.write("build/drop.txt", "x")
		w.write("build/keep.txt", "x")
		w.expect("create_directory build", "create_file build/keep.txt", "create_file keep.tmp")
	})

	t.Run("a trailing slash matches directories only", func(t *testing.T) {
		w := start(t, factory, []seed{{path: rules, body: "out/\n"}}, &execenv.IgnoreRules{File: rules})
		w.write("out", "a file named out")
		w.mkdir("sub")
		w.mkdir("sub/out")
		w.write("sub/out/x", "x")
		w.expect("create_directory sub", "create_file out")
	})

	t.Run("a leading slash anchors to the workspace root", func(t *testing.T) {
		w := start(t, factory, []seed{{path: rules, body: "/root.txt\ndocs/*.md\n"}}, &execenv.IgnoreRules{File: rules})
		w.write("root.txt", "x")
		w.mkdir("sub")
		w.write("sub/root.txt", "x")
		w.mkdir("docs")
		w.write("docs/a.md", "x")
		w.write("docs/a.txt", "x")
		w.mkdir("sub/docs")
		w.write("sub/docs/a.md", "x")
		w.expect("create_directory docs", "create_directory sub", "create_directory sub/docs", "create_file docs/a.txt", "create_file sub/docs/a.md", "create_file sub/root.txt")
	})

	t.Run("double star spans any depth", func(t *testing.T) {
		w := start(t, factory, []seed{{path: rules, body: "**/cache\nlogs/**\n"}}, &execenv.IgnoreRules{File: rules})
		w.mkdir("a")
		w.mkdir("a/b")
		w.mkdir("a/b/cache")
		w.write("a/b/cache/x", "x")
		w.mkdir("cache")
		w.mkdir("logs")
		w.write("logs/x", "x")
		w.expect("create_directory a", "create_directory a/b", "create_directory logs")
	})

	t.Run("an ignore file edit takes effect for the next scan", func(t *testing.T) {
		w := start(t, factory, []seed{{path: rules, body: "*.log\n"}}, &execenv.IgnoreRules{File: rules})
		w.write("a.log", "x")
		w.expect()
		// A projected edit of the ignore file is not itself an observation.
		w.project(rules, "*.tmp\n")
		w.write("b.tmp", "x")
		w.expect("create_file a.log")
		// A guest edit of the ignore file.
		w.write(rules, "*.log\n")
		w.write("c.tmp", "x")
		w.write("d.log", "x")
		w.expect("create_file b.tmp", "create_file c.tmp", "write_file "+rules)
	})

	t.Run("a missing or unparseable ignore file uses the fallback", func(t *testing.T) {
		w := start(t, factory, nil, &execenv.IgnoreRules{File: rules, Fallback: []string{"*.log"}})
		w.write("a.log", "x")
		w.write("b.txt", "x")
		w.expect("create_file b.txt")
		w.write(rules, "[unterminated\n")
		w.write("c.log", "x")
		w.expect("create_file " + rules)
		w.write(rules, "*.txt\n")
		w.write("d.txt", "x")
		w.expect("create_file a.log", "create_file c.log", "write_file "+rules)
	})

	t.Run("the ignore file and its parents are always observed", func(t *testing.T) {
		w := start(t, factory, nil, &execenv.IgnoreRules{File: "config/" + rules, Fallback: []string{"*"}})
		w.mkdir("config")
		w.write("config/"+rules, "*\nconfig/\n")
		w.write("config/other", "x")
		w.write("top", "x")
		w.expect("create_directory config", "create_file config/"+rules)
		w.write("config/"+rules, "*\n")
		w.expect("write_file config/" + rules)
	})

	t.Run("an ignored directory costs nothing even in bulk", func(t *testing.T) {
		w := start(t, factory, []seed{{path: rules, body: "deps/\n"}}, &execenv.IgnoreRules{File: rules})
		w.mkdir("deps")
		for i := range 2000 {
			w.write(fmt.Sprintf("deps/f%04d", i), "x")
		}
		w.expect()
		w.write("ok.txt", "x")
		w.expect("create_file ok.txt")
	})
}

type seed struct {
	path, body string
	dir        bool
}

type watcher struct {
	t          *testing.T
	o          Observed
	workspace  uint64
	host       uint64
	versions   map[string]string
	entries    map[string]string
	generation int
	// seen and applied serve the observation subtests: what drain read,
	// and which apply ids were committed.
	seen    map[string]execenv.ExecutionObservation
	applied map[string]bool
}

func start(t *testing.T, factory ObservedFactory, seeds []seed, rules *execenv.IgnoreRules) *watcher {
	t.Helper()
	o := factory(t)
	w := &watcher{t: t, o: o, workspace: 10, versions: map[string]string{}, entries: map[string]string{}, applied: map[string]bool{}}
	request := execenv.ProjectionInitialization{Fence: o.Fence, MutationID: "init", WorkspaceCursor: w.workspace, Ignore: rules}
	for i, s := range seeds {
		entry := execenv.ProjectionEntry{EntryID: fmt.Sprintf("entry-%d", i), Path: s.path, Kind: execenv.ProjectionDirectory}
		if !s.dir {
			ref, err := o.Workspace.UploadProjectionContent(t.Context(), o.Fence, strings.NewReader(s.body))
			if err != nil {
				t.Fatal(err)
			}
			entry.Kind, entry.ContentVersion, entry.Content = execenv.ProjectionFile, "v0", &ref
			w.versions[s.path] = "v0"
		}
		w.entries[s.path] = entry.EntryID
		request.Entries = append(request.Entries, entry)
	}
	if _, err := o.Workspace.InitializeProjection(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *watcher) write(path, body string) {
	w.t.Helper()
	if err := w.o.Guest.Write(w.t.Context(), path, []byte(body)); err != nil {
		w.t.Fatalf("guest write %s: %v", path, err)
	}
}
func (w *watcher) mkdir(path string) {
	w.t.Helper()
	if err := w.o.Guest.Mkdir(w.t.Context(), path); err != nil {
		w.t.Fatalf("guest mkdir %s: %v", path, err)
	}
}
func (w *watcher) move(from, to string) {
	w.t.Helper()
	if err := w.o.Guest.Move(w.t.Context(), from, to); err != nil {
		w.t.Fatalf("guest move %s: %v", from, err)
	}
}

// project writes a projected file through the caller's journal.
func (w *watcher) project(path, body string) {
	w.t.Helper()
	ref, err := w.o.Workspace.UploadProjectionContent(w.t.Context(), w.o.Fence, strings.NewReader(body))
	if err != nil {
		w.t.Fatal(err)
	}
	w.generation++
	next := fmt.Sprintf("v%d", w.generation)
	request := execenv.ProjectionApply{Fence: w.o.Fence, MutationID: "apply-" + next, FromWorkspaceCursor: w.workspace, ThroughWorkspaceCursor: w.workspace + 1, ExpectedHostCursor: w.host, Mutations: []execenv.ProjectionMutation{{
		JournalCursor: w.workspace + 1, EntryID: w.entries[path], Operation: execenv.ProjectionWrite, Kind: execenv.ProjectionFile, Path: path,
		ExpectedContentVersion: execenv.Version(w.versions[path]), ResultingContentVersion: execenv.Version(next), Content: &ref,
	}}}
	if _, err := w.o.Workspace.ApplyProjection(w.t.Context(), request); err != nil {
		w.t.Fatalf("project %s: %v", path, err)
	}
	w.workspace++
	w.versions[path] = next
}

// expect drains every new observation and compares it, unordered, to want.
func (w *watcher) expect(want ...string) {
	w.t.Helper()
	var got []string
	for {
		page, err := w.o.Workspace.Observe(w.t.Context(), execenv.ObserveRequest{Fence: w.o.Fence, After: w.host, Limit: execenv.MaxProjectionMutations})
		if err != nil {
			if errors.Is(err, execenv.ErrLagged) {
				w.t.Fatalf("observation lagged: %v", err)
			}
			w.t.Fatal(err)
		}
		for _, event := range page.Observations {
			text := string(event.Operation) + " " + event.Path
			if event.DestinationPath != "" {
				text += " -> " + event.DestinationPath
			}
			got = append(got, text)
			w.host = event.HostSequence
		}
		if len(page.Observations) == 0 {
			break
		}
	}
	slices.Sort(got)
	want = slices.Clone(want)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		w.t.Fatalf("observations = %q, want %q", got, want)
	}
}
