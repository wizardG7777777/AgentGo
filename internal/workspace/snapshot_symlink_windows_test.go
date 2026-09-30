package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestCopyProjectTreePreservesDirectoryLinkBeforeTarget(t *testing.T) {
	source := t.TempDir()
	packageRoot := filepath.Join(source, "packages", "fixture")
	if err := os.MkdirAll(packageRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packageRoot, "index.js"), []byte("baseline"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(source, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	relative := filepath.Join("..", "packages", "fixture")
	if err := os.Symlink(relative, filepath.Join(source, "node_modules", "fixture")); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "candidate")
	if err := copyProjectTree(source, destination); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(destination, "node_modules", "fixture")
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	attributes := info.Sys().(*syscall.Win32FileAttributeData)
	if attributes.FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY == 0 {
		t.Fatal("copied directory symlink became a file symlink before its target was copied")
	}
	if value, err := os.Readlink(link); err != nil || value != relative {
		t.Fatalf("relative target changed: %q, %v", value, err)
	}
	if err := os.WriteFile(filepath.Join(destination, "packages", "fixture", "index.js"), []byte("candidate"), 0o644); err != nil {
		t.Fatal(err)
	}
	if value, err := os.ReadFile(filepath.Join(link, "index.js")); err != nil || string(value) != "candidate" {
		t.Fatalf("link must read the candidate: %q, %v", value, err)
	}
	if value, err := os.ReadFile(filepath.Join(packageRoot, "index.js")); err != nil || string(value) != "baseline" {
		t.Fatalf("candidate change reached the original root: %q, %v", value, err)
	}
}

func snapshotSymlinkInfo(t *testing.T, directory bool) os.FileInfo {
	t.Helper()
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if directory {
		if err := os.Mkdir(target, 0o755); err != nil {
			t.Fatal(err)
		}
	} else if err := os.WriteFile(target, []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink("target", link); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestCopySnapshotSymlinkKeepsDanglingDirectoryType(t *testing.T) {
	info := snapshotSymlinkInfo(t, true)
	root := t.TempDir()
	link := filepath.Join(root, "link")
	if err := copySnapshotSymlink(info, "missing", link); err != nil {
		t.Fatal(err)
	}
	copied, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if copied.Sys().(*syscall.Win32FileAttributeData).FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY == 0 {
		t.Fatal("dangling directory link lost its directory attribute")
	}
	if _, err := os.Stat(filepath.Join(root, "missing")); !os.IsNotExist(err) {
		t.Fatalf("copy must not create the link target: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "missing"), 0o755); err != nil {
		t.Fatal(err)
	}
	if value, err := os.Stat(link); err != nil || !value.IsDir() {
		t.Fatalf("directory link unusable after target appears: %v", err)
	}
}

func TestCopySnapshotSymlinkKeepsFileTypeAndDoesNotReplaceDestination(t *testing.T) {
	info := snapshotSymlinkInfo(t, false)
	root := t.TempDir()
	link := filepath.Join(root, "link")
	if err := copySnapshotSymlink(info, "later.txt", link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "later.txt"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if value, err := os.ReadFile(link); err != nil || string(value) != "content" {
		t.Fatalf("file link unusable: %q, %v", value, err)
	}
	if err := copySnapshotSymlink(info, "different.txt", link); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing destination must reject replacement: %v", err)
	}
	if value, err := os.Readlink(link); err != nil || value != "later.txt" {
		t.Fatalf("existing link changed: %q, %v", value, err)
	}
}

func TestCopySnapshotSymlinkSupportsLongPathsAndRelativeContents(t *testing.T) {
	info := snapshotSymlinkInfo(t, true)
	root := t.TempDir()
	for len(root) < 300 {
		root = filepath.Join(root, strings.Repeat("segment", 6))
	}
	if err := os.MkdirAll(filepath.Join(root, "destination"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"destination", filepath.Join(root, "destination")} {
		name := "relative-link"
		if filepath.IsAbs(target) {
			name = "absolute-link"
		}
		link := filepath.Join(root, name)
		if err := copySnapshotSymlink(info, target, link); err != nil {
			t.Fatal(err)
		}
		if value, err := os.Stat(link); err != nil || !value.IsDir() {
			t.Fatalf("long directory link unusable: %v", err)
		}
		if value, err := os.Readlink(link); err != nil || value != target {
			t.Fatalf("link content changed: %q want %q, %v", value, target, err)
		}
	}
}

func TestCopySnapshotSymlinkReturnsPermissionFailureWithoutDroppingDirectoryFlag(t *testing.T) {
	info := snapshotSymlinkInfo(t, true)
	calls := 0
	err := copySnapshotSymlinkWith(info, "missing", filepath.Join(t.TempDir(), "link"), func(_, _ *uint16, flags uint32) error {
		calls++
		if flags&syscall.SYMBOLIC_LINK_FLAG_DIRECTORY == 0 {
			t.Fatal("source directory type missing from native call")
		}
		return syscall.ERROR_ACCESS_DENIED
	})
	if !errors.Is(err, syscall.ERROR_ACCESS_DENIED) || calls != 1 {
		t.Fatalf("permission error must remain an error: calls=%d err=%v", calls, err)
	}
}

func TestCopySnapshotSymlinkOldWindowsRetryKeepsDirectoryFlag(t *testing.T) {
	info := snapshotSymlinkInfo(t, true)
	var flagsSeen []uint32
	err := copySnapshotSymlinkWith(info, "missing", filepath.Join(t.TempDir(), "link"), func(_, _ *uint16, flags uint32) error {
		flagsSeen = append(flagsSeen, flags)
		if len(flagsSeen) == 1 {
			return snapshotSymlinkInvalidParameter
		}
		return nil
	})
	if err != nil || len(flagsSeen) != 2 || flagsSeen[0] != 3 || flagsSeen[1] != 1 {
		t.Fatalf("unexpected native retry: %v, %v", flagsSeen, err)
	}
}

func TestSnapshotLinkWin32PathKeepsNamespaceAndUNC(t *testing.T) {
	for input, expected := range map[string]string{
		`C:\tree\link`:             `\\?\C:\tree\link`,
		`\\server\share\tree\link`: `\\?\UNC\server\share\tree\link`,
		`\\?\C:\tree\link`:         `\\?\C:\tree\link`,
		`\\?\UNC\host\share\link`:  `\\?\UNC\host\share\link`,
	} {
		value, err := snapshotLinkWin32Path(input)
		if err != nil || value != expected {
			t.Fatalf("namespace conversion %q: %q want %q, %v", input, value, expected, err)
		}
	}
}
