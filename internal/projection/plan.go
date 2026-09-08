package projection

import (
	"bytes"
	"maps"
	"strings"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/guesttree"
	"github.com/sudosylabs/execenv/internal/tree"
)

type step struct {
	before, after map[string]Node
	mutation      execenv.Mutation
	echo          bool
}

// Plan validates actual guest identities and bytes for every non-echo mutation
// against its position in the consecutive journal, before any disk write.
type Plan struct{ steps []step }
type Prepared struct {
	Tree  tree.Snapshot
	Guest *guesttree.Snapshot
	Batch execenv.Batch
}

func (p Plan) Prepare(actual *guesttree.Snapshot) (Prepared, error) {
	if actual == nil {
		return Prepared{}, execenv.ErrConflict
	}
	current := maps.Clone(actual.Nodes)
	batch := execenv.Batch{}
	for _, step := range p.steps {
		if step.echo {
			continue
		}
		if err := checkActual(current, step.before, step.mutation); err != nil {
			return Prepared{}, err
		}
		before := physicalTree(current, step.before)
		next, err := tree.Apply(before, execenv.Batch{Mutations: []execenv.Mutation{step.mutation}})
		if err != nil {
			return Prepared{}, err
		}
		identities := maps.Clone(current)
		m := step.mutation
		if m.Op == execenv.OpMove {
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
			if name == m.Path && (m.Op == execenv.OpCreate || m.Op == execenv.OpReplace) {
				identity = step.after[name].Identity
			}
			if identity == "" {
				return Prepared{}, execenv.ErrLagged
			}
			kind := execenv.ProjectionFile
			if node.Kind == execenv.KindDirectory {
				kind = execenv.ProjectionDirectory
			}
			current[name] = guesttree.Node{Identity: identity, Kind: kind, Data: bytes.Clone(node.Data)}
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
		if kind == execenv.KindFile {
			version = "guest"
			if expected, ok := baseline[name]; ok && sameActual(node, expected) {
				version = expected.Version
			}
		}
		result[name] = tree.Node{Kind: kind, Version: version, Data: bytes.Clone(node.Data)}
	}
	return result
}
func sameActual(actual guesttree.Node, expected Node) bool {
	return actual.Identity == expected.Identity && actual.Kind == expected.Kind && bytes.Equal(actual.Data, expected.Data)
}
func checkActual(actual map[string]guesttree.Node, baseline map[string]Node, m execenv.Mutation) error {
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
