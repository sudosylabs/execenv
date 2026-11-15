package projection

import (
	"maps"
	"path"
	"strings"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/guesttree"
	"github.com/sudosylabs/execenv/internal/tree"
)

type step struct {
	before, after map[string]Node
	mutation      tree.Mutation
	echo          bool
	// cursor is the mutation's journal cursor.
	cursor uint64
}

// Plan validates actual guest identities and bytes for every non-echo mutation
// against its position in the consecutive journal, before any disk write.
// An overwriting mutation instead replaces what the guest left on its path.
type Plan struct {
	steps  []step
	result *planResult
}

// planResult is what the commit reports back for the receipt.
type planResult struct{ discarded []uint64 }

// Overwrite is one overwriting mutation: the journal cursor it has and the
// path whose subtree it replaces.
type Overwrite struct {
	JournalCursor uint64
	Path          string
}

// Overwrites lists the plan's overwriting mutations in journal order.
func (p Plan) Overwrites() []Overwrite {
	var out []Overwrite
	for _, step := range p.steps {
		if step.mutation.Overwrite && !step.echo {
			out = append(out, Overwrite{JournalCursor: step.cursor, Path: step.mutation.Path})
		}
	}
	return out
}

// Discard records, in order, the retained observations the commit
// discarded; the receipt lists them.
func (p Plan) Discard(sequences []uint64) {
	if p.result != nil {
		p.result.discarded = sequences
	}
}

type Prepared struct {
	Tree  tree.Snapshot
	Guest *guesttree.Snapshot
	Batch tree.Batch
}

func (p Plan) Prepare(actual *guesttree.Snapshot) (Prepared, error) {
	if actual == nil {
		return Prepared{}, execenv.ErrConflict
	}
	current := maps.Clone(actual.Nodes)
	batch := tree.Batch{}
	for _, step := range p.steps {
		if step.echo {
			continue
		}
		if step.mutation.Overwrite {
			if err := checkOverwrite(current, step.before, step.mutation); err != nil {
				return Prepared{}, err
			}
			// What the guest left in the subtree is replaced: plan as if the
			// projection's own subtree were there.
			current = withBaseline(current, step.before, step.mutation.Path)
		} else if err := checkActual(current, step.before, step.mutation); err != nil {
			return Prepared{}, err
		}
		before := physicalTree(current, step.before)
		next, err := tree.Apply(before, tree.Batch{Mutations: []tree.Mutation{step.mutation}})
		if err != nil {
			return Prepared{}, err
		}
		identities := maps.Clone(current)
		m := step.mutation
		if m.Op == tree.OpMove {
			for name, node := range current {
				if name == m.From || strings.HasPrefix(name, m.From+"/") {
					delete(identities, name)
					identities[m.Path+strings.TrimPrefix(name, m.From)] = node
				}
			}
		}
		current = make(map[string]guesttree.Node, len(next))
		for name, node := range next {
			identity := identities[name].Identity
			if name == m.Path && (m.Op == tree.OpCreate || m.Op == tree.OpReplace) {
				identity = step.after[name].Identity
			}
			if identity == "" {
				return Prepared{}, execenv.ErrLagged
			}
			kind := execenv.ProjectionFile
			if node.Kind == tree.KindDirectory {
				kind = execenv.ProjectionDirectory
			}
			current[name] = guesttree.Node{Identity: identity, Kind: kind, Content: node.Content}
		}
		batch.Mutations = append(batch.Mutations, m)
	}
	baseline := map[string]Node{}
	if len(p.steps) > 0 {
		baseline = p.steps[len(p.steps)-1].after
	}
	return Prepared{Tree: physicalTree(current, baseline), Guest: &guesttree.Snapshot{Nodes: current}, Batch: batch}, nil
}
func physicalTree(nodes map[string]guesttree.Node, baseline map[string]Node) tree.Snapshot {
	result := make(tree.Snapshot, len(nodes))
	for name, node := range nodes {
		kind := nodeKind(node.Kind)
		version := execenv.Version("")
		if kind == tree.KindFile {
			version = "guest"
			if expected, ok := baseline[name]; ok && sameActual(node, expected) {
				version = expected.Version
			}
		}
		result[name] = tree.Node{Kind: kind, Version: version, Content: node.Content}
	}
	return result
}
func sameActual(actual guesttree.Node, expected Node) bool {
	return actual.Identity == expected.Identity && actual.Kind == expected.Kind && actual.Content == expected.Content
}
func under(name, root string) bool { return name == root || strings.HasPrefix(name, root+"/") }

// checkOverwrite refuses an overwrite the guest's changes are not confined
// to: its parent is gone or no longer a directory, a projected path moved
// out from under it, or a projected path from elsewhere moved into it.
func checkOverwrite(actual map[string]guesttree.Node, baseline map[string]Node, m tree.Mutation) error {
	if parent := path.Dir(m.Path); parent != "." {
		node, ok := actual[parent]
		if !ok || node.Kind != execenv.ProjectionDirectory {
			return execenv.ErrConflict
		}
	}
	outside := make(map[string]bool)
	for name, node := range actual {
		if !under(name, m.Path) {
			outside[node.Identity] = true
		}
	}
	projectedOutside := make(map[string]bool)
	for name, node := range baseline {
		if under(name, m.Path) {
			if outside[node.Identity] {
				return execenv.ErrConflict
			}
		} else {
			projectedOutside[node.Identity] = true
		}
	}
	for name, node := range actual {
		if under(name, m.Path) && projectedOutside[node.Identity] {
			return execenv.ErrConflict
		}
	}
	return nil
}

// withBaseline is actual with root's subtree replaced by the projection's.
func withBaseline(actual map[string]guesttree.Node, baseline map[string]Node, root string) map[string]guesttree.Node {
	out := make(map[string]guesttree.Node, len(actual))
	for name, node := range actual {
		if !under(name, root) {
			out[name] = node
		}
	}
	for name, node := range baseline {
		if under(name, root) {
			out[name] = guesttree.Node{Identity: node.Identity, Kind: node.Kind, Content: node.Content}
		}
	}
	return out
}

func checkActual(actual map[string]guesttree.Node, baseline map[string]Node, m tree.Mutation) error {
	affected := func(name string) bool {
		for _, root := range []string{m.Path, m.From} {
			if root != "" && (name == root || strings.HasPrefix(name, root+"/")) {
				return true
			}
		}
		return false
	}
	for name, expected := range baseline {
		if affected(name) {
			node, ok := actual[name]
			if !ok || !sameActual(node, expected) {
				return execenv.ErrConflict
			}
		}
	}
	for name := range actual {
		if affected(name) {
			if _, ok := baseline[name]; !ok {
				return execenv.ErrConflict
			}
		}
	}
	return nil
}
