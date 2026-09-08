package agent

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/sudosylabs/execenv"
	"github.com/sudosylabs/execenv/internal/tree"
)

// checkProjectionDisk refuses guest changes already present in any affected
// path/subtree. It is a precondition check, not a guest-write serialization
// barrier; the stronger fenced projection protocol still requires that barrier.
func checkProjectionDisk(home string, projected tree.Snapshot, batch execenv.Batch) error {
	roots := make(map[string]struct{})
	for _, mutation := range batch.Mutations {
		roots[mutation.Path] = struct{}{}
		if mutation.Op == execenv.OpMove {
			roots[mutation.From] = struct{}{}
		}
	}
	expected := make(map[string]tree.Node)
	for path, node := range projected {
		for root := range roots {
			if path == root || strings.HasPrefix(path, root+"/") {
				expected[path] = node
				break
			}
		}
	}
	// ReplaceTree permits implicit directory entries. Include those implied
	// ancestors, but never infer an unprojected guest file as caller authority.
	for path := range expected {
		for parent := filepath.ToSlash(filepath.Dir(path)); parent != "."; parent = filepath.ToSlash(filepath.Dir(parent)) {
			for root := range roots {
				if parent == root || strings.HasPrefix(parent, root+"/") {
					if _, ok := expected[parent]; !ok {
						expected[parent] = tree.Node{Kind: execenv.KindDirectory}
					}
					break
				}
			}
		}
	}
	seen := make(map[string]struct{})
	var total int64
	for root := range roots {
		full, err := execenv.ResolvePath(home, root)
		if err != nil {
			return err
		}
		if _, err := os.Lstat(full); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		err = filepath.WalkDir(full, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			relative, err := filepath.Rel(home, path)
			if err != nil {
				return err
			}
			relative = filepath.ToSlash(relative)
			if _, ok := seen[relative]; ok {
				if entry.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			seen[relative] = struct{}{}
			node, ok := expected[relative]
			if !ok {
				return execenv.ErrConflict
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return execenv.ErrConflict
			}
			if node.Kind == execenv.KindDirectory {
				if !entry.IsDir() {
					return execenv.ErrConflict
				}
				return nil
			}
			if !entry.Type().IsRegular() {
				return execenv.ErrConflict
			}
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			body, readErr := io.ReadAll(io.LimitReader(file, execenv.MaxFileBytes+1))
			closeErr := file.Close()
			if readErr != nil {
				return readErr
			}
			if closeErr != nil {
				return closeErr
			}
			total += int64(len(body))
			if len(body) > execenv.MaxFileBytes || total > execenv.MaxTreeBytes {
				return execenv.ErrTooLarge
			}
			if !bytes.Equal(body, node.Data) {
				return execenv.ErrConflict
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	for path := range expected {
		if _, ok := seen[path]; !ok {
			return execenv.ErrConflict
		}
	}
	return nil
}
