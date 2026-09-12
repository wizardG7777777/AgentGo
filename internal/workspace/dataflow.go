package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const CandidateSchema = "agentgo.candidate/v1"
const dataflowBaseName = ".workspace-input"

type CandidateFile struct {
	SHA256     string `json:"sha256"`
	BaseSHA256 string `json:"base_sha256,omitempty"`
	Deleted    bool   `json:"deleted,omitempty"`
}
type FrozenCandidate struct {
	TreeDigest string                   `json:"tree_digest"`
	Schema     string                   `json:"schema"`
	Ref        string                   `json:"ref"`
	GraphID    string                   `json:"graph_id"`
	RunID      string                   `json:"run_id"`
	ParentRef  string                   `json:"parent_ref,omitempty"`
	Files      map[string]CandidateFile `json:"files"`
}

func snapshotIgnored(rel string, dir bool) bool {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for _, part := range parts {
		if part == ".git" || part == ".agentgo" || part == ".venv" || part == "__pycache__" || part == ".pytest_cache" {
			return true
		}
	}
	return !dir && (strings.HasSuffix(rel, ".pyc") || strings.HasSuffix(rel, ".pyo"))
}

func snapshotHashes(root string) (map[string]string, error) {
	result := map[string]string{}
	err := filepath.WalkDir(root, func(path string, e fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if snapshotIgnored(rel, e.IsDir()) {
			if e.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if e.IsDir() {
			return nil
		}
		if !e.Type().IsRegular() {
			return fmt.Errorf("候选不支持非普通项目文件 %s", rel)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		result[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	return result, err
}

func (m *Manager) candidatePath(ref string) (string, error) {
	id := strings.TrimPrefix(ref, "candidate:")
	if id == ref || len(id) != 64 {
		return "", fmt.Errorf("候选引用格式非法")
	}
	if _, err := hex.DecodeString(id); err != nil {
		return "", fmt.Errorf("候选引用格式非法")
	}
	return filepath.Join(m.projectRoot, ".agentgo", "candidates-v1", id), nil
}

func (m *Manager) ReadCandidate(ref, graphID, runID string) (FrozenCandidate, string, error) {
	var c FrozenCandidate
	root, err := m.candidatePath(ref)
	if err != nil {
		return c, "", err
	}
	data, err := os.ReadFile(filepath.Join(root, "candidate.json"))
	if err != nil {
		return c, "", err
	}
	if err = json.Unmarshal(data, &c); err != nil {
		return c, "", err
	}
	if c.Schema != CandidateSchema || c.Ref != ref || c.GraphID != graphID || c.RunID != runID {
		return c, "", fmt.Errorf("候选版本或作用域不一致")
	}
	expected := c.Ref
	c.Ref = ""
	raw, _ := json.Marshal(c)
	sum := sha256.Sum256(raw)
	c.Ref = expected
	if "candidate:"+hex.EncodeToString(sum[:]) != expected {
		return c, "", fmt.Errorf("候选元数据摘要不一致")
	}
	tree := filepath.Join(root, "tree")
	hashes, err := snapshotHashes(tree)
	if err != nil {
		return c, "", err
	}
	treeRaw, _ := json.Marshal(hashes)
	treeSum := sha256.Sum256(treeRaw)
	if hex.EncodeToString(treeSum[:]) != c.TreeDigest {
		return c, "", fmt.Errorf("候选完整目录被更改")
	}
	for path, file := range c.Files {
		hash, exists := hashes[path]
		if file.Deleted {
			if exists {
				return c, "", fmt.Errorf("候选删除事实不一致: %s", path)
			}
		} else if !exists || hash != file.SHA256 {
			return c, "", fmt.Errorf("候选内容被更改: %s", path)
		}
	}
	return c, tree, nil
}

func (m *Manager) MaterializeAgentTask(taskID, graphID, runID, parentRef string) (*View, error) {
	if graphID == "" {
		return m.Materialize(taskID)
	}
	view, err := m.Materialize(taskID)
	if err != nil {
		return nil, err
	}
	view.mu.Lock()
	defer view.mu.Unlock()
	base := filepath.Join(view.root, dataflowBaseName)
	if _, err := os.Stat(base); os.IsNotExist(err) {
		source := m.projectRoot
		if parentRef != "" {
			_, source, err = m.ReadCandidate(parentRef, graphID, runID)
			if err != nil {
				return nil, err
			}
		}
		tmp, err := os.MkdirTemp(view.root, ".input-build-")
		if err != nil {
			return nil, err
		}
		if err = copyProjectTree(source, tmp); err != nil {
			_ = removeTree(tmp)
			return nil, err
		}
		if err = os.Rename(tmp, base); err != nil {
			_ = removeTree(tmp)
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	view.baseRoot = base
	return view, nil
}

// CaptureShellChanges 把 Shell 的实际文件差异同步回同一任务 overlay。
func (v *View) CaptureShellChanges() error {
	if v == nil || v.baseRoot == "" {
		return nil
	}
	v.shellMu.Lock()
	defer v.shellMu.Unlock()
	if !v.shellReady {
		return nil
	}
	root := filepath.Join(v.root, shellRootDirName)
	before, err := snapshotHashes(v.baseRoot)
	if err != nil {
		return err
	}
	after, err := snapshotHashes(root)
	if err != nil {
		return err
	}
	paths := map[string]bool{}
	for path := range before {
		paths[path] = true
	}
	for path := range after {
		paths[path] = true
	}
	for rel := range v.mf.snapshot() {
		paths[filepath.ToSlash(rel)] = true
	}
	for path := range paths {
		rel := filepath.FromSlash(path)
		if before[path] == after[path] {
			if _, dirty := v.mf.get(rel); dirty {
				if err := os.Remove(filepath.Join(v.root, rel)); err != nil && !os.IsNotExist(err) {
					return err
				}
				if err := v.mf.remove(rel); err != nil {
					return err
				}
			}
			continue
		}
		entry := manifestEntry{BaselineSHA256: before[path], New: before[path] == ""}
		if old, ok := v.mf.get(rel); ok {
			entry = old
		}
		target := filepath.Join(v.root, rel)
		if after[path] == "" {
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				return err
			}
		} else {
			info, err := os.Stat(filepath.Join(root, rel))
			if err != nil {
				return err
			}
			if err = copySnapshotFile(filepath.Join(root, rel), target, info.Mode().Perm()); err != nil {
				return err
			}
		}
		if err := v.mf.set(rel, entry); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) FreezeAgentTaskCandidate(taskID, graphID, runID, parentRef string) (string, error) {
	v := m.ActiveView(taskID)
	if v == nil {
		return "", fmt.Errorf("节点工作区未物化")
	}
	root, err := v.PrepareShellRoot()
	if err != nil {
		return "", err
	}
	base, err := snapshotHashes(v.baseRoot)
	if err != nil {
		return "", err
	}
	current, err := snapshotHashes(root)
	if err != nil {
		return "", err
	}
	files := map[string]CandidateFile{}
	if parentRef != "" {
		parent, _, err := m.ReadCandidate(parentRef, graphID, runID)
		if err != nil {
			return "", err
		}
		for path, file := range parent.Files {
			files[path] = file
		}
	}
	changed := false
	paths := map[string]bool{}
	for path := range base {
		paths[path] = true
	}
	for path := range current {
		paths[path] = true
	}
	for path := range paths {
		if base[path] == current[path] {
			continue
		}
		changed = true
		original := base[path]
		if old, ok := files[path]; ok {
			original = old.BaseSHA256
		}
		files[path] = CandidateFile{SHA256: current[path], BaseSHA256: original, Deleted: current[path] == ""}
	}
	if !changed {
		return parentRef, nil
	}
	c := FrozenCandidate{Schema: CandidateSchema, GraphID: graphID, RunID: runID, ParentRef: parentRef, Files: files}
	treeRaw, _ := json.Marshal(current)
	treeSum := sha256.Sum256(treeRaw)
	c.TreeDigest = hex.EncodeToString(treeSum[:])
	raw, _ := json.Marshal(c)
	sum := sha256.Sum256(raw)
	c.Ref = "candidate:" + hex.EncodeToString(sum[:])
	dest, _ := m.candidatePath(c.Ref)
	if _, err := os.Stat(dest); err == nil {
		_, _, err = m.ReadCandidate(c.Ref, graphID, runID)
		return c.Ref, err
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err = os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dest), ".candidate-build-")
	if err != nil {
		return "", err
	}
	if err = copyProjectTree(root, filepath.Join(tmp, "tree")); err == nil {
		raw, err = json.Marshal(c)
		if err == nil {
			err = os.WriteFile(filepath.Join(tmp, "candidate.json"), raw, 0600)
		}
	}
	if err == nil {
		err = os.Rename(tmp, dest)
	}
	if err != nil {
		_ = removeTree(tmp)
		return "", err
	}
	return c.Ref, nil
}

func (m *Manager) CandidateChanges(ref, graphID, runID string) (FrozenCandidate, string, error) {
	return m.ReadCandidate(ref, graphID, runID)
}

// ApplyCandidate 只接受与原始基线一致的目标；调用方先记录 Effect prepared。
func (m *Manager) ApplyCandidate(ref, graphID, runID string) error {
	m.commitMu.Lock()
	defer m.commitMu.Unlock()
	c, tree, err := m.ReadCandidate(ref, graphID, runID)
	if err != nil {
		return err
	}
	paths := make([]string, 0, len(c.Files))
	for p := range c.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	if err := m.checkCandidateBaseline(c, paths); err != nil {
		return err
	}
	for _, p := range paths {
		f := c.Files[p]
		target := filepath.Join(m.projectRoot, filepath.FromSlash(p))
		if f.Deleted {
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				return err
			}
			continue
		}
		info, err := os.Stat(filepath.Join(tree, filepath.FromSlash(p)))
		if err != nil {
			return err
		}
		if err = copySnapshotFile(filepath.Join(tree, filepath.FromSlash(p)), target, info.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) CheckCandidateCommit(ref, graphID, runID string) error {
	m.commitMu.Lock()
	defer m.commitMu.Unlock()
	c, _, err := m.ReadCandidate(ref, graphID, runID)
	if err != nil {
		return err
	}
	paths := make([]string, 0, len(c.Files))
	for p := range c.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return m.checkCandidateBaseline(c, paths)
}
func (m *Manager) checkCandidateBaseline(c FrozenCandidate, paths []string) error {
	for _, p := range paths {
		f := c.Files[p]
		target := filepath.Join(m.projectRoot, filepath.FromSlash(p))
		body, err := os.ReadFile(target)
		current := ""
		if err == nil {
			sum := sha256.Sum256(body)
			current = hex.EncodeToString(sum[:])
		} else if !os.IsNotExist(err) {
			return err
		}
		if current != f.BaseSHA256 {
			return fmt.Errorf("交付基线冲突: %s", p)
		}
	}

	return nil
}
