//go:build !windows

package workspace

import "os"

func copySnapshotSymlink(_ os.FileInfo, link, target string) error {
	return os.Symlink(link, target)
}
