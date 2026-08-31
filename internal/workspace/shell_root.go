package workspace

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const shellRootDirName = ".workspace-shell"

// prepareShellRoot 把主根复制为可丢弃的完整项目快照，再以
// manifest dirty set 覆盖。.agentgo/.git 是控制面与 VCS 元数据，
// 不得递归进快照（shell root 自身位于 .agentgo 内）。
func (v *View) prepareShellRoot() (string, error) {
	if v == nil || v.mgr == nil {
		return "", fmt.Errorf("workspace shell root 缺少 View/Manager")
	}
	v.shellMu.Lock()
	defer v.shellMu.Unlock()
	root := filepath.Join(v.root, shellRootDirName)
	if !v.shellReady {
		if err := removeTree(root); err != nil {
			return "", fmt.Errorf("清理旧 shell snapshot: %w", err)
		}
		tmp, err := os.MkdirTemp(v.root, ".workspace-shell-build-")
		if err != nil {
			return "", fmt.Errorf("创建 shell snapshot 临时目录: %w", err)
		}
		cleanup := func() { _ = removeTree(tmp) }
		if err := copyProjectTree(v.mgr.projectRoot, tmp); err != nil {
			cleanup()
			return "", fmt.Errorf("物化 shell 项目快照: %w", err)
		}
		if err := v.syncDirtyInto(tmp); err != nil {
			cleanup()
			return "", err
		}
		if err := os.Rename(tmp, root); err != nil {
			cleanup()
			return "", fmt.Errorf("发布 shell 项目快照: %w", err)
		}
		v.shellReady = true
	}
	if err := v.syncDirtyInto(root); err != nil {
		return "", err
	}
	return root, nil
}

func (v *View) discardShellRoot() error {
	if v == nil {
		return nil
	}
	v.shellMu.Lock()
	defer v.shellMu.Unlock()
	err := removeTree(filepath.Join(v.root, shellRootDirName))
	if err == nil {
		v.shellReady = false
	}
	return err
}

func copyProjectTree(sourceRoot, targetRoot string) error {
	return filepath.WalkDir(sourceRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(sourceRoot, path)
		if err != nil || rel == "." {
			return err
		}
		first := rel
		if index := strings.IndexRune(rel, filepath.Separator); index >= 0 {
			first = rel[:index]
		}
		if first == ".agentgo" || first == ".git" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(targetRoot, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case entry.Type()&os.ModeSymlink != 0:
			link, readErr := os.Readlink(path)
			if readErr != nil {
				return readErr
			}
			if mkErr := os.MkdirAll(filepath.Dir(target), 0o755); mkErr != nil {
				return mkErr
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			return copySnapshotFile(path, target, info.Mode().Perm())
		default:
			return fmt.Errorf("项目含不支持的特殊文件 %s mode=%s", rel, info.Mode())
		}
	})
}

func (v *View) syncDirtyInto(targetRoot string) error {
	entries := v.mf.snapshot()
	rels := make([]string, 0, len(entries))
	for rel := range entries {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		source := filepath.Join(v.root, rel)
		target := filepath.Join(targetRoot, rel)
		info, err := os.Stat(source)
		if os.IsNotExist(err) {
			if removeErr := os.Remove(target); removeErr != nil && !os.IsNotExist(removeErr) {
				return fmt.Errorf("同步 shell snapshot 删除 %s: %w", rel, removeErr)
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("同步 shell snapshot 读取 %s: %w", rel, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("同步 shell snapshot 读取 %s: dirty entry 不是普通文件", rel)
		}
		if err := copySnapshotFile(source, target, info.Mode().Perm()); err != nil {
			return fmt.Errorf("同步 shell snapshot %s: %w", rel, err)
		}
	}
	return nil
}

func copySnapshotFile(source, target string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	// Windows 的只读目标不允许 O_TRUNC；这里只处理我们自己
	// 物化的可丢弃快照，先恢复可写位再原子覆盖内容。
	if _, statErr := os.Stat(target); statErr == nil {
		_ = os.Chmod(target, 0o666)
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		_ = in.Close()
		return err
	}
	_, copyErr := io.Copy(out, in)
	inErr := in.Close()
	outErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if inErr != nil {
		return inErr
	}
	if outErr != nil {
		return outErr
	}
	return os.Chmod(target, mode)
}

func removeTree(root string) error {
	if err := os.RemoveAll(root); err == nil {
		return nil
	}
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, _ error) error {
		if entry != nil && !entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
			_ = os.Chmod(path, 0o666)
		}
		return nil
	})
	return os.RemoveAll(root)
}
