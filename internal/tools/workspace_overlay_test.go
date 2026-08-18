package tools

// workspace_overlay_test.go 覆盖「按任务写时复制执行隔离」在工具层的行为：
// Workdir 实现 PathOverlayer 时 read_file/write_file/edit_file 的读写位置
// 解析、roster 跳过、trace 逻辑路径保持；ActiveViewer 对 run_shell 默认
// 工作目录的切换。fakeOverlayer 忠实模拟 workspace.View 的语义
// （读穿透 + copy-on-write），与 internal/workspace 的真实现解耦。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"agentgo/internal/agent"
	"agentgo/internal/shell"
	"agentgo/internal/trace"
	"agentgo/internal/workspace"
)

// fakeOverlayer 同时实现 WorkdirProvider 与 PathOverlayer：
// Get 恒返回主根；ReadPath 命中 overlayDir 内副本则返回副本，否则穿透主根；
// WritePath 返回 overlayDir 内路径并对已有主根文件做 copy-on-write。
type fakeOverlayer struct {
	mainRoot   string
	overlayDir string
	writeErr   error // 非 nil 时 WritePath 返回该错误
	readCalls  int
	writeCalls int
}

func (f *fakeOverlayer) Get() string { return f.mainRoot }

func (f *fakeOverlayer) ReadPath(absMainPath string) string {
	f.readCalls++
	rel, err := filepath.Rel(f.mainRoot, absMainPath)
	if err != nil {
		return absMainPath
	}
	cand := filepath.Join(f.overlayDir, rel)
	if _, err := os.Stat(cand); err == nil {
		return cand
	}
	return absMainPath
}

func (f *fakeOverlayer) WritePath(absMainPath string) (string, error) {
	f.writeCalls++
	if f.writeErr != nil {
		return "", f.writeErr
	}
	rel, err := filepath.Rel(f.mainRoot, absMainPath)
	if err != nil {
		return "", fmt.Errorf("路径不在主根内: %w", err)
	}
	wsPath := filepath.Join(f.overlayDir, rel)
	if _, err := os.Stat(wsPath); err == nil {
		return wsPath, nil
	}
	// copy-on-write：主根已有文件先复制基线进 overlay（新文件的父目录由
	// write_file 的 MkdirAll 负责，与真实现的分工一致）。
	if data, rerr := os.ReadFile(absMainPath); rerr == nil {
		if err := os.MkdirAll(filepath.Dir(wsPath), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(wsPath, data, 0o644); err != nil {
			return "", err
		}
	}
	return wsPath, nil
}

func newOverlayGroups(t *testing.T) (LocalWriteGroup, *recordingRoster, *fakeOverlayer, string, string) {
	t.Helper()
	mainRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	overlayDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rr := &recordingRoster{}
	ov := &fakeOverlayer{mainRoot: mainRoot, overlayDir: overlayDir}
	g := LocalWriteGroup{
		LocalReadGroup: LocalReadGroup{
			Workdir: ov,
			Cache:   agent.NewFileStateCache(10),
		},
		Roster:  rr,
		AgentID: "agent-1",
	}
	return g, rr, ov, mainRoot, overlayDir
}

// read_file：overlay 中已有副本时读副本内容（写后读一致性），
// 且 FileStateCache 以解析后的物理路径为键。
func TestReadFile_OverlayReadsCopy(t *testing.T) {
	g, _, ov, mainRoot, overlayDir := newOverlayGroups(t)

	mainPath := filepath.Join(mainRoot, "a.txt")
	if err := os.WriteFile(mainPath, []byte("主根内容"), 0o644); err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(overlayDir, "a.txt")
	if err := os.WriteFile(copyPath, []byte("副本内容"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := g.readFile(context.Background(), map[string]any{"path": mainPath})
	if err != nil {
		t.Fatalf("readFile: %v", err)
	}
	if !strings.Contains(out, "副本内容") || strings.Contains(out, "主根内容") {
		t.Fatalf("应读到 overlay 副本内容，实际输出:\n%s", out)
	}
	if ov.readCalls == 0 {
		t.Fatal("read_file 未经 PathOverlayer.ReadPath 解析")
	}
	// 缓存键 = 解析后的物理路径：以副本路径 Get 必须命中。
	if _, _, ok := g.Cache.Get(copyPath); !ok {
		t.Fatal("FileStateCache 应以物理副本路径为键（Get 未命中）")
	}
}

// read_file：overlay 无副本时穿透主根实时内容。
func TestReadFile_OverlayPassthroughWhenNoCopy(t *testing.T) {
	g, _, _, mainRoot, _ := newOverlayGroups(t)

	mainPath := filepath.Join(mainRoot, "b.txt")
	if err := os.WriteFile(mainPath, []byte("主根实时内容"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := g.readFile(context.Background(), map[string]any{"path": mainPath})
	if err != nil {
		t.Fatalf("readFile: %v", err)
	}
	if !strings.Contains(out, "主根实时内容") {
		t.Fatalf("无副本时应穿透主根，实际输出:\n%s", out)
	}
}

// write_file：隔离生效时——写入落 overlay、主根不动、跳过 roster 全流程，
// 且 file_written 事件的 Path 保持主根逻辑路径。
func TestWriteFile_OverlaySkipsRosterAndKeepsLogicalTracePath(t *testing.T) {
	d := &captureGraphTraceDispatcher{}
	original := trace.DefaultDispatcher()
	trace.SetDefaultDispatcher(d)
	t.Cleanup(func() { trace.SetDefaultDispatcher(original) })

	g, rr, ov, mainRoot, overlayDir := newOverlayGroups(t)

	rel := filepath.Join("sub", "new.txt")
	mainPath := filepath.Join(mainRoot, rel)
	out, err := callWriteFile(g, mainPath, "隔离写入内容", "")
	if err != nil {
		t.Fatalf("writeFile: %v", err)
	}

	// 落点在 overlay，主根无此文件。
	data, err := os.ReadFile(filepath.Join(overlayDir, rel))
	if err != nil || string(data) != "隔离写入内容" {
		t.Fatalf("overlay 落点内容不符: data=%q err=%v", data, err)
	}
	if _, err := os.Stat(mainPath); !os.IsNotExist(err) {
		t.Fatalf("隔离写入不应落主根: stat err=%v", err)
	}
	// 跳过 roster：无任何 TryClaim/Release/WaitForRelease。
	if events := rr.snapshot(); len(events) != 0 {
		t.Fatalf("隔离生效时不应触碰 roster，实际事件: %v", events)
	}
	if ov.writeCalls != 1 {
		t.Fatalf("WritePath 调用次数 = %d，want 1", ov.writeCalls)
	}
	// 返回消息保持主根逻辑坐标。
	if !strings.Contains(out, mainPath) {
		t.Fatalf("返回消息应含主根逻辑路径 %s，实际: %s", mainPath, out)
	}
	// trace Path 保持逻辑路径，Description 注明 workspace 落点。
	var written []trace.Event
	for _, ev := range d.events {
		if ev.Kind == trace.KindFileWritten {
			written = append(written, ev)
		}
	}
	if len(written) != 1 {
		t.Fatalf("file_written 事件数 = %d，want 1", len(written))
	}
	if written[0].Path != mainPath {
		t.Fatalf("trace Path 应保持主根逻辑路径 %s，实际: %s", mainPath, written[0].Path)
	}
	if !strings.Contains(written[0].Description, overlayDir) {
		t.Fatalf("trace Description 应注明 workspace 落点（%s），实际: %q", overlayDir, written[0].Description)
	}
}

// edit_file：隔离生效时经 copy-on-write 编辑副本，主根原文件不变，
// 跳过 roster，trace Path 保持逻辑路径。
func TestEditFile_OverlayEditsCopyNotMain(t *testing.T) {
	d := &captureGraphTraceDispatcher{}
	original := trace.DefaultDispatcher()
	trace.SetDefaultDispatcher(d)
	t.Cleanup(func() { trace.SetDefaultDispatcher(original) })

	g, rr, _, mainRoot, overlayDir := newOverlayGroups(t)

	mainPath := filepath.Join(mainRoot, "c.txt")
	if err := os.WriteFile(mainPath, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := callEditFile(g, mainPath, "world", "workspace", "")
	if err != nil {
		t.Fatalf("editFile: %v", err)
	}

	// 副本被编辑，主根原样。
	data, err := os.ReadFile(filepath.Join(overlayDir, "c.txt"))
	if err != nil || string(data) != "hello workspace" {
		t.Fatalf("overlay 副本内容不符: data=%q err=%v", data, err)
	}
	mainData, err := os.ReadFile(mainPath)
	if err != nil || string(mainData) != "hello world" {
		t.Fatalf("主根文件不应被改动: data=%q err=%v", mainData, err)
	}
	if events := rr.snapshot(); len(events) != 0 {
		t.Fatalf("隔离生效时不应触碰 roster，实际事件: %v", events)
	}
	if !strings.Contains(out, mainPath) {
		t.Fatalf("返回消息应含主根逻辑路径，实际: %s", out)
	}
	var written []trace.Event
	for _, ev := range d.events {
		if ev.Kind == trace.KindFileWritten {
			written = append(written, ev)
		}
	}
	if len(written) != 1 || written[0].Path != mainPath {
		t.Fatalf("trace Path 应保持主根逻辑路径 %s，实际: %+v", mainPath, written)
	}
}

// WritePath 返回错误时：工具返回错误、roster 未被触碰、无落盘。
func TestWriteFile_OverlayWritePathError(t *testing.T) {
	g, rr, ov, mainRoot, _ := newOverlayGroups(t)
	ov.writeErr = fmt.Errorf("模拟 overlay 故障")

	mainPath := filepath.Join(mainRoot, "d.txt")
	_, err := callWriteFile(g, mainPath, "内容", "")
	if err == nil || !strings.Contains(err.Error(), "解析隔离写入位置失败") {
		t.Fatalf("应返回「解析隔离写入位置失败」错误，实际: %v", err)
	}
	if events := rr.snapshot(); len(events) != 0 {
		t.Fatalf("WritePath 失败不应触碰 roster，实际事件: %v", events)
	}
	if _, serr := os.Stat(mainPath); !os.IsNotExist(serr) {
		t.Fatalf("WritePath 失败不应有落盘: stat err=%v", serr)
	}
}

// 无 overlay（DefaultWorkdir）时 write_file 行为不变：走 roster 声明、
// 直写主根。与既有测试互补的显式回归护栏。
func TestWriteFile_NoOverlayClaimsRosterAsBefore(t *testing.T) {
	g, rr, tmp := newWriteGroup(t, nil)

	mainPath := filepath.Join(tmp, "plain.txt")
	if _, err := callWriteFile(g, mainPath, "普通写入", ""); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	events := rr.snapshot()
	if len(events) != 2 ||
		!strings.HasPrefix(events[0], "TryClaim:") ||
		!strings.HasPrefix(events[1], "Release:") {
		t.Fatalf("无 overlay 时应按 TryClaim→Release 声明 roster，实际: %v", events)
	}
	data, err := os.ReadFile(mainPath)
	if err != nil || string(data) != "普通写入" {
		t.Fatalf("主根落盘内容不符: data=%q err=%v", data, err)
	}
}

// run_shell：ActiveViewer 有活动视图时默认工作目录切到视图根；
// 无视图时回退主根（Swapper.Get）。
func TestRunShell_DefaultWorkdirFollowsActiveView(t *testing.T) {
	mainRoot := t.TempDir()
	swapper := workspace.NewSwapper(mainRoot)

	group := ShellGroup{
		Workdir:      swapper,
		TimeoutSec:   10,
		Interactions: nil, // pwd 不命中灰名单，无需 Interaction 服务
		AgentID:      "test-agent",
		Filter:       shell.NewCommandFilter(nil, nil),
		ActiveViewer: swapper,
	}

	// 打印当前目录的命令按 shell 方言分支：PowerShell 的 pwd / echo $PWD 输出
	// PathInfo 对象走 Format-Table（长路径被折行截断，不可断言），取 $PWD.Path
	// 纯字符串；POSIX sh 直接用 pwd。
	cwdCmd := "pwd"
	if runtime.GOOS == "windows" {
		cwdCmd = "echo $PWD.Path"
	}
	containsFold := func(haystack, needle string) bool {
		return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
	}

	// 无视图：默认目录为主根。
	out, err := dispatchRunShell(context.Background(), group, map[string]any{"command": cwdCmd})
	if err != nil {
		t.Fatalf("run_shell(无视图): %v", err)
	}
	mainCanonical := swapper.MainRoot()
	if !containsFold(out, mainCanonical) {
		t.Fatalf("无视图时默认目录应为主根 %s，实际输出:\n%s", mainCanonical, out)
	}

	// 有视图：默认目录为 workspace 视图根（真实 Manager 物化，
	// root=<另一主根>/.agentgo/workspaces/<taskID>，与 Swapper 主根必然不同）。
	mgr := workspace.NewManager(t.TempDir(), nil)
	view, err := mgr.Materialize("task-shell")
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	restore := swapper.Activate(view)
	defer restore()

	out, err = dispatchRunShell(context.Background(), group, map[string]any{"command": cwdCmd})
	if err != nil {
		t.Fatalf("run_shell(有视图): %v", err)
	}
	if !containsFold(out, view.Root()) {
		t.Fatalf("有视图时默认目录应为视图根 %s，实际输出:\n%s", view.Root(), out)
	}

	// 隔离视图生效时，显式 working_dir 也只能位于该任务 workspace 内，
	// 不能切回主根绕过隔离。
	_, err = dispatchRunShell(context.Background(), group,
		map[string]any{"command": cwdCmd, "working_dir": mainRoot})
	if err == nil || !strings.Contains(err.Error(), "working_dir 被拒绝") {
		t.Fatalf("隔离任务显式切回主根必须拒绝，实际 err=%v", err)
	}
}
