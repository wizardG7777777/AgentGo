package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Windows records whether a symlink names a directory in the link itself.
// os.Symlink infers that bit from the destination target, which may not have
// been copied yet. Preserve the source metadata without following the link.
func copySnapshotSymlink(info os.FileInfo, link, target string) error {
	return copySnapshotSymlinkWith(info, link, target, syscall.CreateSymbolicLink)
}

const (
	snapshotSymlinkAllowUnprivilegedCreate = 0x2
	snapshotSymlinkInvalidParameter        = syscall.Errno(87) // ERROR_INVALID_PARAMETER
)

func copySnapshotSymlinkWith(info os.FileInfo, link, target string, create func(*uint16, *uint16, uint32) error) error {
	wrap := func(err error) error {
		return &os.LinkError{Op: "symlink", Old: link, New: target, Err: err}
	}
	if info == nil || info.Mode()&os.ModeSymlink == 0 {
		return wrap(syscall.EINVAL)
	}
	attributes, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok || attributes == nil {
		return wrap(syscall.EINVAL)
	}
	flags := uint32(snapshotSymlinkAllowUnprivilegedCreate)
	if attributes.FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY != 0 {
		flags |= syscall.SYMBOLIC_LINK_FLAG_DIRECTORY
	}
	destination, err := snapshotLinkWin32Path(target)
	if err != nil {
		return wrap(err)
	}
	newName, err := syscall.UTF16PtrFromString(destination)
	if err != nil {
		return wrap(err)
	}
	oldName := filepath.FromSlash(link)
	if filepath.IsAbs(oldName) {
		oldName, err = snapshotLinkWin32Path(oldName)
		if err != nil {
			return wrap(err)
		}
	}
	old, err := syscall.UTF16PtrFromString(oldName)
	if err != nil {
		return wrap(err)
	}
	err = create(newName, old, flags)
	if errors.Is(err, snapshotSymlinkInvalidParameter) {
		// Older Windows does not recognize ALLOW_UNPRIVILEGED_CREATE. Retry
		// without that flag, preserving the directory bit and current token.
		err = create(newName, old, flags&^snapshotSymlinkAllowUnprivilegedCreate)
	}
	if err != nil {
		return wrap(err)
	}
	return nil
}

func snapshotLinkWin32Path(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	if strings.HasPrefix(absolute, `\\?\`) || strings.HasPrefix(absolute, `\\.\`) {
		return absolute, nil
	}
	// This only selects the Win32 path namespace. Relative link contents are
	// never made absolute, and directory components are joined by filepath.
	if strings.HasPrefix(absolute, `\\`) {
		return filepath.Join(`\\?\UNC`, strings.TrimPrefix(absolute, `\\`)), nil
	}
	return filepath.Join(`\\?\`, absolute), nil
}
