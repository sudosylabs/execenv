//go:build !unix && !windows

package files

import (
	"errors"
	"os"
)

const nonblock = 0

func renameAt(*os.File, string, *os.File, string) error { return errors.ErrUnsupported }
