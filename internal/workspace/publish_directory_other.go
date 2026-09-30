//go:build !windows

package workspace

import "os"

func publishDirectory(source, destination string) error {
	return os.Rename(source, destination)
}
