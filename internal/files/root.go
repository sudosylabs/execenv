package files

import (
	"os"

	"github.com/sudosylabs/execenv"
)

func openDirectory(root *os.Root, name string) (*os.Root, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, notFound(err)
	}
	if !info.IsDir() {
		return nil, execenv.ErrNotFound
	}
	dir, err := root.OpenRoot(name)
	return dir, notFound(err)
}

// OpenRegular opens the same regular file described by listed without
// blocking if a concurrent writer replaced it with a special file.
func OpenRegular(root *os.Root, name string, listed os.FileInfo) (*os.File, error) {
	file, err := root.OpenFile(name, os.O_RDONLY|nonblock, 0)
	if err != nil {
		return nil, err
	}
	actual, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !actual.Mode().IsRegular() || !os.SameFile(listed, actual) {
		_ = file.Close()
		return nil, execenv.ErrConflict
	}
	return file, nil
}

// publish moves a staged entry between two already-resolved directories.
// Neither pathname can resolve another directory after its handle was opened.
func publish(src *os.Root, old string, dst *os.Root, newName string) error {
	from, err := src.Open(".")
	if err != nil {
		return err
	}
	defer from.Close()
	to, err := dst.Open(".")
	if err != nil {
		return err
	}
	defer to.Close()
	return renameAt(from, old, to, newName)
}
