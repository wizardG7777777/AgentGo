package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Windows 纪律：全部文件读写用 os.WriteFile / os.ReadFile（内部自行关闭
// 句柄），无长生命周期句柄，t.TempDir() 可安全清理。

func newTestManager(t *testing.T) (*Manager, string) {
	t.Helper()
	root := t.TempDir()
	// nil roster：跳过声明（仅测试用，见 Manager 注释）。
	m := NewManager(root, nil)
	return m, m.ProjectRoot()
}

func writeMain(t *testing.T, root, rel, content string) string {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("创建目录失败：%v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("写主根文件失败：%v", err)
	}
	return p
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取文件失败 %s：%v", path, err)
	}
	return string(data)
}

// cowAndEdit 走一遍完整 copy-on-write：Materialize → WritePath → 在
// workspace 副本上写入新内容。返回 workspace 副本路径。
func cowAndEdit(t *testing.T, m *Manager, taskID, mainPath, newContent string) string {
	t.Helper()
	v, err := m.Materialize(taskID)
	if err != nil {
		t.Fatalf("Materialize 失败：%v", err)
	}
	wsPath, err := v.WritePath(mainPath)
	if err != nil {
		t.Fatalf("WritePath 失败：%v", err)
	}
	if err := os.WriteFile(wsPath, []byte(newContent), 0o644); err != nil {
		t.Fatalf("写 workspace 副本失败：%v", err)
	}
	return wsPath
}

func merge(t *testing.T, m *Manager, taskID string) *MergeResult {
	t.Helper()
	res, err := m.MergeTask(context.Background(), taskID, "agent-1")
	if err != nil {
		t.Fatalf("MergeTask 失败：%v", err)
	}
	return res
}

func TestWritePathCopyOnWrite(t *testing.T) {
	m, root := newTestManager(t)
	mainPath := writeMain(t, root, filepath.Join("src", "a.txt"), "hello\n")
	v, err := m.Materialize("task-1")
	if err != nil {
		t.Fatalf("Materialize 失败：%v", err)
	}

	// 首次 WritePath 触发 copy-on-write。
	wsPath, err := v.WritePath(mainPath)
	if err != nil {
		t.Fatalf("WritePath 失败：%v", err)
	}
	wantWs := filepath.Join(root, DirName, "task-1", "src", "a.txt")
	if wsPath != wantWs {
		t.Fatalf("WritePath 返回路径错误：得到 %s 期望 %s", wsPath, wantWs)
	}
	if got := readFile(t, wsPath); got != "hello\n" {
		t.Fatalf("workspace 副本内容错误：%q", got)
	}

	// manifest 记录基线 SHA256 且已持久化到磁盘。
	sum := sha256.Sum256([]byte("hello\n"))
	data, err := os.ReadFile(filepath.Join(root, DirName, "task-1", ManifestFileName))
	if err != nil {
		t.Fatalf("manifest 未持久化：%v", err)
	}
	var entries map[string]manifestEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("manifest 解析失败：%v", err)
	}
	e, ok := entries[filepath.Join("src", "a.txt")]
	if !ok {
		t.Fatalf("manifest 未登记目标文件")
	}
	if e.New {
		t.Fatalf("已有文件不应标记为新建")
	}
	if e.BaselineSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("基线哈希错误：%s", e.BaselineSHA256)
	}

	// 主根未被改动；重复 WritePath 幂等返回同一路径。
	if got := readFile(t, mainPath); got != "hello\n" {
		t.Fatalf("主根不应被改动：%q", got)
	}
	wsPath2, err := v.WritePath(mainPath)
	if err != nil || wsPath2 != wsPath {
		t.Fatalf("重复 WritePath 应幂等返回同一路径：%s %v", wsPath2, err)
	}

	// ReadPath：命中副本返回 workspace 路径，未触碰文件穿透主根。
	if got := v.ReadPath(mainPath); got != wsPath {
		t.Fatalf("ReadPath 应返回 workspace 副本：%q", got)
	}
	other := writeMain(t, root, filepath.Join("src", "b.txt"), "b\n")
	if got := v.ReadPath(other); got != other {
		t.Fatalf("未触碰文件应穿透主根：%q", got)
	}

	// manifest 未登记但 workspace 下文件存在时，ReadPath 同样命中副本。
	lonely := filepath.Join(root, DirName, "task-1", "lonely.txt")
	if err := os.WriteFile(lonely, []byte("x\n"), 0o644); err != nil {
		t.Fatalf("写文件失败：%v", err)
	}
	if got := v.ReadPath(filepath.Join(root, "lonely.txt")); got != lonely {
		t.Fatalf("workspace 已有副本应优先返回：%q", got)
	}
}

func TestWritePathNewFile(t *testing.T) {
	m, root := newTestManager(t)
	v, err := m.Materialize("task-nf")
	if err != nil {
		t.Fatalf("Materialize 失败：%v", err)
	}
	mainPath := filepath.Join(root, "docs", "new.md")
	wsPath, err := v.WritePath(mainPath)
	if err != nil {
		t.Fatalf("WritePath 失败：%v", err)
	}
	e, ok := v.mf.get(filepath.Join("docs", "new.md"))
	if !ok {
		t.Fatalf("manifest 未登记新建文件")
	}
	if !e.New || e.BaselineSHA256 != "" {
		t.Fatalf("新建文件应 New=true 且基线为空串：%+v", e)
	}
	// 父目录已创建，文件尚未写入。
	if _, err := os.Stat(filepath.Dir(wsPath)); err != nil {
		t.Fatalf("workspace 父目录应已创建：%v", err)
	}
	if _, err := os.Stat(wsPath); !os.IsNotExist(err) {
		t.Fatalf("WritePath 不应代写文件内容")
	}
}

func TestMergeNewFile(t *testing.T) {
	m, root := newTestManager(t)
	mainPath := filepath.Join(root, "docs", "new.md")
	cowAndEdit(t, m, "task-new", mainPath, "brand new\n")

	res := merge(t, m, "task-new")
	if res.Conflicted {
		t.Fatalf("新建文件不应冲突：%+v", res.Reports)
	}
	if len(res.Reports) != 1 || res.Reports[0].Outcome != OutcomeNewFile {
		t.Fatalf("应为 OutcomeNewFile：%+v", res.Reports)
	}
	if got := readFile(t, mainPath); got != "brand new\n" {
		t.Fatalf("新建文件未落盘主根：%q", got)
	}
}

func TestMergeFastForward(t *testing.T) {
	m, root := newTestManager(t)
	mainPath := writeMain(t, root, "a.txt", "v1\n")
	cowAndEdit(t, m, "task-ff", mainPath, "v2\n")

	res := merge(t, m, "task-ff")
	if res.Conflicted {
		t.Fatalf("fast-forward 不应冲突：%+v", res.Reports)
	}
	if len(res.Reports) != 1 || res.Reports[0].Outcome != OutcomeFastForward {
		t.Fatalf("应为 OutcomeFastForward：%+v", res.Reports)
	}
	if got := readFile(t, mainPath); got != "v2\n" {
		t.Fatalf("fast-forward 后主根内容错误：%q", got)
	}
}

func TestMergeAutoMerged(t *testing.T) {
	m, root := newTestManager(t)
	mainPath := writeMain(t, root, "a.txt", "l1\nl2\nl3\nl4\nl5\n")
	// workspace 改第 5 行；随后主根被他人改第 1 行（模拟并发变更）。
	cowAndEdit(t, m, "task-am", mainPath, "l1\nl2\nl3\nl4\nL5\n")
	if err := os.WriteFile(mainPath, []byte("L1\nl2\nl3\nl4\nl5\n"), 0o644); err != nil {
		t.Fatalf("写主根文件失败：%v", err)
	}

	res := merge(t, m, "task-am")
	if res.Conflicted {
		t.Fatalf("不相交变更不应冲突：%+v", res.Reports)
	}
	if len(res.Reports) != 1 || res.Reports[0].Outcome != OutcomeAutoMerged {
		t.Fatalf("应为 OutcomeAutoMerged：%+v", res.Reports)
	}
	if got := readFile(t, mainPath); got != "L1\nl2\nl3\nl4\nL5\n" {
		t.Fatalf("自动合并结果错误：%q", got)
	}
}

func TestMergeConflictNotWritten(t *testing.T) {
	m, root := newTestManager(t)
	mainPath := writeMain(t, root, "a.txt", "a\nb\nc\n")
	cowAndEdit(t, m, "task-cf", mainPath, "a\nOURS\nc\n")
	// 主根同位置并发修改 → 行区间相交。
	if err := os.WriteFile(mainPath, []byte("a\nMAIN\nc\n"), 0o644); err != nil {
		t.Fatalf("写主根文件失败：%v", err)
	}

	res := merge(t, m, "task-cf")
	if !res.Conflicted {
		t.Fatalf("相交变更应报冲突：%+v", res.Reports)
	}
	paths := res.ConflictedPaths()
	if len(paths) != 1 || paths[0] != mainPath {
		t.Fatalf("ConflictedPaths 错误：%v", paths)
	}
	rep := res.Reports[0]
	if rep.Outcome != OutcomeConflict || len(rep.Conflicts) != 1 {
		t.Fatalf("冲突报告错误：%+v", rep)
	}
	if rep.Conflicts[0].Main != "MAIN" || rep.Conflicts[0].Workspace != "OURS" {
		t.Fatalf("冲突文本错误：%+v", rep.Conflicts[0])
	}
	// 冲突文件不落地：主根保留合并前内容。
	if got := readFile(t, mainPath); got != "a\nMAIN\nc\n" {
		t.Fatalf("冲突文件不应落盘，主根内容被改：%q", got)
	}
}

func TestMergeDeleteVsModify(t *testing.T) {
	m, root := newTestManager(t)
	mainPath := writeMain(t, root, "a.txt", "a\nb\n")
	cowAndEdit(t, m, "task-del", mainPath, "a\nB\n")
	if err := os.Remove(mainPath); err != nil {
		t.Fatalf("删除主根文件失败：%v", err)
	}

	res := merge(t, m, "task-del")
	if !res.Conflicted {
		t.Fatalf("删除-vs-修改应报冲突：%+v", res.Reports)
	}
	rep := res.Reports[0]
	if rep.Outcome != OutcomeConflict {
		t.Fatalf("应为 OutcomeConflict：%+v", rep)
	}
	if rep.Detail == "" || !strings.Contains(rep.Detail, "删除") {
		t.Fatalf("Detail 应注明删除-vs-修改：%q", rep.Detail)
	}
}

func TestMaterializeIdempotentAndReload(t *testing.T) {
	m, root := newTestManager(t)
	v1, err := m.Materialize("task-x")
	if err != nil {
		t.Fatalf("Materialize 失败：%v", err)
	}
	v2, err := m.Materialize("task-x")
	if err != nil {
		t.Fatalf("Materialize 失败：%v", err)
	}
	if v1 != v2 {
		t.Fatalf("Materialize 应幂等返回同一活动视图")
	}

	// 登记一次 COW 后，换一个新 Manager（模拟重试/重启）重新 Materialize，
	// manifest 应从磁盘恢复。
	mainPath := writeMain(t, root, "k.txt", "base\n")
	if _, err := v1.WritePath(mainPath); err != nil {
		t.Fatalf("WritePath 失败：%v", err)
	}
	m2 := NewManager(root, nil)
	v3, err := m2.Materialize("task-x")
	if err != nil {
		t.Fatalf("重新 Materialize 失败：%v", err)
	}
	if _, ok := v3.mf.get("k.txt"); !ok {
		t.Fatalf("manifest 未从磁盘恢复")
	}
	if got := v3.ReadPath(mainPath); got != filepath.Join(root, DirName, "task-x", "k.txt") {
		t.Fatalf("重载后 ReadPath 未命中 workspace 副本：%q", got)
	}
}

func TestListOrphans(t *testing.T) {
	m, root := newTestManager(t)
	orphans, err := m.ListOrphans()
	if err != nil || orphans != nil {
		t.Fatalf("workspace 根不存在应返回 nil, nil：%v %v", orphans, err)
	}

	for _, id := range []string{"task-a", "task-b"} {
		if _, err := m.Materialize(id); err != nil {
			t.Fatalf("Materialize 失败：%v", err)
		}
	}
	orphans, err = m.ListOrphans()
	if err != nil {
		t.Fatalf("ListOrphans 失败：%v", err)
	}
	sort.Strings(orphans)
	if len(orphans) != 2 || orphans[0] != "task-a" || orphans[1] != "task-b" {
		t.Fatalf("孤儿列表错误：%v", orphans)
	}
	// 换一个 Manager 实例（无内存状态）也应看到同样的目录。
	m2 := NewManager(root, nil)
	orphans2, err := m2.ListOrphans()
	if err != nil || len(orphans2) != 2 {
		t.Fatalf("跨实例 ListOrphans 错误：%v %v", orphans2, err)
	}
}

func TestCleanup(t *testing.T) {
	m, root := newTestManager(t)
	if _, err := m.Materialize("task-c"); err != nil {
		t.Fatalf("Materialize 失败：%v", err)
	}
	wsRoot := filepath.Join(root, DirName, "task-c")

	if err := m.Cleanup("task-c"); err != nil {
		t.Fatalf("Cleanup 失败：%v", err)
	}
	if _, err := os.Stat(wsRoot); !os.IsNotExist(err) {
		t.Fatalf("workspace 目录应已删除")
	}
	if m.ActiveView("task-c") != nil {
		t.Fatalf("活动视图应已注销")
	}
	orphans, err := m.ListOrphans()
	if err != nil {
		t.Fatalf("ListOrphans 失败：%v", err)
	}
	for _, o := range orphans {
		if o == "task-c" {
			t.Fatalf("已清理目录不应再列为孤儿")
		}
	}
}

func TestCleanupGuard(t *testing.T) {
	m, root := newTestManager(t)
	// 在 workspace 根外预建目录，验证防卫逻辑不会误删。
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("创建目录失败：%v", err)
	}

	for _, bad := range []string{"../outside", `..\outside`, "a/b", "", "."} {
		if err := m.Cleanup(bad); err == nil {
			t.Fatalf("越界 taskID %q 应被拒绝", bad)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("workspace 根外目录不应被删除：%v", err)
	}
}

// --- 双任务并发写同一主根文件（fan-out 写集相交，本特性的存在理由） ---

// TestMergeTask_TwoIsolatedTasksDisjointEditsAutoMerge 菱形场景：两个隔离任务
// 同时对同一文件做 COW（基线同为原版），各自修改不相交的行。先合并者
// fast-forward；后合并者面对「主根自基线已变」走三路自动合并——最终主根
// 必须同时携带两处修改，且两份报告分别为 fast_forward / auto_merged。
func TestMergeTask_TwoIsolatedTasksDisjointEditsAutoMerge(t *testing.T) {
	m, root := newTestManager(t)
	rel := "shared.go"
	mainPath := writeMain(t, root, rel, "line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\n")

	// 两个任务先后物化（基线都是原版），各自改不相交的行。
	v1, err := m.Materialize("task-A")
	if err != nil {
		t.Fatalf("Materialize A: %v", err)
	}
	v2, err := m.Materialize("task-B")
	if err != nil {
		t.Fatalf("Materialize B: %v", err)
	}
	wsA, err := v1.WritePath(mainPath)
	if err != nil {
		t.Fatalf("WritePath A: %v", err)
	}
	wsB, err := v2.WritePath(mainPath)
	if err != nil {
		t.Fatalf("WritePath B: %v", err)
	}
	if wsA == mainPath || wsB == mainPath {
		t.Fatal("隔离生效时 WritePath 不应原样返回主根路径")
	}
	editLines(t, wsA, 2, "line2-taskA")
	editLines(t, wsB, 8, "line8-taskB")

	// task-A 先合并：主根未变 → fast-forward。
	resA, err := m.MergeTask(context.Background(), "task-A", "agent-A")
	if err != nil {
		t.Fatalf("MergeTask A: %v", err)
	}
	if resA.Conflicted {
		t.Fatalf("A 不应冲突: %+v", resA.Reports)
	}
	if got := resA.Reports[0].Outcome; got != OutcomeFastForward {
		t.Fatalf("A 的 outcome = %s，want fast_forward", got)
	}

	// task-B 后合并：主根已被 A 改过（hash ≠ 基线），三路合并区间不相交 → auto_merged。
	resB, err := m.MergeTask(context.Background(), "task-B", "agent-B")
	if err != nil {
		t.Fatalf("MergeTask B: %v", err)
	}
	if resB.Conflicted {
		t.Fatalf("B 不应冲突（行不相交）: %+v", resB.Reports)
	}
	if got := resB.Reports[0].Outcome; got != OutcomeAutoMerged {
		t.Fatalf("B 的 outcome = %s，want auto_merged", got)
	}

	// 终态：主根同时携带 A、B 两处修改，其余行保持原版。
	data, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatalf("读主根: %v", err)
	}
	content := string(data)
	for _, want := range []string{"line2-taskA", "line8-taskB", "line1", "line5"} {
		if !strings.Contains(content, want) {
			t.Fatalf("合并后主根缺少 %q，实际:\n%s", want, content)
		}
	}
}

// TestMergeTask_TwoIsolatedTasksSameLineConflict 双任务改同一行：先合并者胜，
// 后合并者冲突——冲突文件不落盘（主根保留先者内容），ConflictedPaths 正确，
// 且后合并任务的 workspace 现场保留（交 Scheduler 裁决/排查）。
func TestMergeTask_TwoIsolatedTasksSameLineConflict(t *testing.T) {
	m, root := newTestManager(t)
	rel := "hot.go"
	mainPath := writeMain(t, root, rel, "a\nb\nc\n")

	v1, _ := m.Materialize("task-hot-1")
	v2, _ := m.Materialize("task-hot-2")
	ws1, err := v1.WritePath(mainPath)
	if err != nil {
		t.Fatalf("WritePath 1: %v", err)
	}
	ws2, err := v2.WritePath(mainPath)
	if err != nil {
		t.Fatalf("WritePath 2: %v", err)
	}
	editLines(t, ws1, 2, "b-first")
	editLines(t, ws2, 2, "b-second")

	res1, err := m.MergeTask(context.Background(), "task-hot-1", "agent-1")
	if err != nil || res1.Conflicted {
		t.Fatalf("先合并者应成功: res=%+v err=%v", res1, err)
	}
	res2, err := m.MergeTask(context.Background(), "task-hot-2", "agent-2")
	if err != nil {
		t.Fatalf("MergeTask 2: %v", err)
	}
	if !res2.Conflicted {
		t.Fatalf("同一行双改必须冲突: %+v", res2.Reports)
	}
	paths := res2.ConflictedPaths()
	if len(paths) != 1 || paths[0] != mainPath {
		t.Fatalf("ConflictedPaths = %v，want [%s]", paths, mainPath)
	}
	// 冲突不落盘：主根保留先合并者内容。
	data, _ := os.ReadFile(mainPath)
	if !strings.Contains(string(data), "b-first") || strings.Contains(string(data), "b-second") {
		t.Fatalf("冲突后主根应保留先者内容，实际:\n%s", data)
	}
	// 冲突区域报告可用（裁决依据）：含双方文本。
	if len(res2.Reports[0].Conflicts) == 0 {
		t.Fatal("冲突报告应含冲突区域")
	}
}

// editLines 把文件的第 n 行（1-based）替换为 content（保持其余行不变）。
func editLines(t *testing.T, path string, n int, content string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("editLines 读 %s: %v", path, err)
	}
	lines := strings.Split(string(data), "\n")
	if n < 1 || n > len(lines) {
		t.Fatalf("editLines 行号 %d 越界（共 %d 行）", n, len(lines))
	}
	lines[n-1] = content
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatalf("editLines 写 %s: %v", path, err)
	}
}

// TestRelativeProjectRoot_EndToEnd 回归（2026-07-27 真实运行事故）：
// 真实配置允许 project_root: "."，Manager/Swapper 构造期必须把根归一为
// 绝对路径——否则工具层经 ValidatePath 归一后的绝对目标路径会让
// filepath.Rel(相对, 绝对) 报错，隔离任务全部「路径不在主根内」失败。
// 单测此前全用 t.TempDir()（恒绝对），漏检该环境差异。
func TestRelativeProjectRoot_EndToEnd(t *testing.T) {
	root := t.TempDir()
	// 切进主根并以 "." 构造（模拟真实配置的相对根）；测试结束恢复 CWD。
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })

	m := NewManager(".", nil)
	sw := NewSwapper(".")

	// 主根已存在的文件，用绝对路径（工具层 ValidatePath 之后的形态）写入。
	mainPath := filepath.Join(root, "rel.txt")
	if err := os.WriteFile(mainPath, []byte("old1\nold2\n"), 0o644); err != nil {
		t.Fatalf("写主根文件: %v", err)
	}

	v, err := m.Materialize("task-rel")
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	// 认领时换入视图（agent 认领隔离任务的真实动作；无视图时 Swapper passthrough）。
	restore := sw.Activate(v)
	defer restore()
	// 经 Swapper（工具实际走的路径）解析写入位置：必须解析到 workspace 副本
	// 而不是报「路径不在主根内」。
	wsPath, err := sw.WritePath(mainPath)
	if err != nil {
		t.Fatalf("相对根下 WritePath 报错（事故复现）: %v", err)
	}
	if wsPath == mainPath {
		t.Fatal("隔离生效时 WritePath 不应原样返回主根路径")
	}
	if err := os.WriteFile(wsPath, []byte("old1\nnew2\n"), 0o644); err != nil {
		t.Fatalf("写 workspace 副本: %v", err)
	}
	// View 根与 Manager 根应为绝对路径。
	if !filepath.IsAbs(v.Root()) || !filepath.IsAbs(m.ProjectRoot()) {
		t.Fatalf("根应为绝对路径: view=%q mgr=%q", v.Root(), m.ProjectRoot())
	}

	res, err := m.MergeTask(context.Background(), "task-rel", "agent-rel")
	if err != nil {
		t.Fatalf("MergeTask: %v", err)
	}
	if res.Conflicted {
		t.Fatalf("不应冲突: %+v", res.Reports)
	}
	data, err := os.ReadFile(mainPath)
	if err != nil || string(data) != "old1\nnew2\n" {
		t.Fatalf("合并后主根内容 = %q err=%v，want old1\nnew2\n", data, err)
	}
}
