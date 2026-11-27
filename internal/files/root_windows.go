package files

import (
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/sudosylabs/execenv"
)

const nonblock = 0

func renameAt(from *os.File, old string, to *os.File, newName string) error {
	for _, name := range []string{old, newName} {
		if !filepath.IsLocal(name) || name == "." || strings.ContainsAny(name, ":/\\") {
			return execenv.ErrInvalid
		}
	}
	name, err := windows.NewNTUnicodeString(old)
	if err != nil {
		return err
	}
	attrs := windows.OBJECT_ATTRIBUTES{
		RootDirectory: windows.Handle(from.Fd()),
		ObjectName:    name,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	attrs.Length = uint32(unsafe.Sizeof(attrs))
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	if err := windows.NtCreateFile(&handle, windows.DELETE|windows.SYNCHRONIZE, &attrs, &status, nil, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN, windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0); err != nil {
		return err
	}
	defer windows.CloseHandle(handle)

	var info struct {
		Replace     bool
		Directory   windows.Handle
		NameBytes   uint32
		Destination [windows.MAX_PATH]uint16
	}
	target, err := windows.UTF16FromString(newName)
	if err != nil {
		return err
	}
	if len(target) > len(info.Destination) {
		return execenv.ErrInvalid
	}
	info.Replace = true
	info.Directory = windows.Handle(to.Fd())
	info.NameBytes = uint32((len(target) - 1) * 2)
	copy(info.Destination[:], target)
	return windows.NtSetInformationFile(handle, &status, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), windows.FileRenameInformation)
}
