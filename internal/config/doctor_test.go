package config

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// writePromptFile 在临时目录写入 prompt 文件并返回路径。
// os.WriteFile 即时关闭句柄，不阻塞 Windows 的 TempDir 清理。
func writePromptFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("写入 prompt 文件失败: %v", err)
	}
	return p
}

// findDiag 报告是否存在满足条件的诊断。
func findDiag(rep *DoctorReport, level DiagLevel, kind, msgSub string) bool {
	for _, d := range rep.Diags {
		if d.Level == level && d.Kind == kind && strings.Contains(d.Message, msgSub) {
			return true
		}
	}
	return false
}

// TestScanPromptToolNames 验证工具名扫描的命中与词边界防误报。
func TestScanPromptToolNames(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []string
	}{
		{"基本命中", "使用 read_file 和 write_file 完成修改", []string{"read_file", "write_file"}},
		{"词边界防误报-前缀", "my_read_file 不是工具名", nil},
		{"词边界防误报-后缀", "read_files 与 write_file_v2 都不是工具名", nil},
		{"中文紧邻命中", "先用read_file读取，再用edit_file修改", []string{"read_file", "edit_file"}},
		{"多次提及去重", "read_file 之后再次 read_file", []string{"read_file"}},
		{"独立词区分", "read_file 与 write_file 是独立词，edit_file 也是", []string{"read_file", "write_file", "edit_file"}},
		{"scheduler 专属工具也算已知名", "report_done 是 scheduler 专属工具", []string{"report_done"}},
		{"空文本", "", nil},
		{"无命中", "这段文字不包含任何工具名", nil},
		// 否定语境启发式：含否定标记的行整行不计入 mentioned
		{"否定行排除-不要", "不要尝试调用 report_done", nil},
		{"否定行排除-没有", "Worker 没有 report_done 工具", nil},
		{"否定行排除-严禁", "严禁主动 send_message 广播", nil},
		{"否定行排除-禁止", "禁止在没有读取的情况下直接 write_file", nil},
		{"条件句不算否定", "只有 plan_id 为空且工具实际可用的兼容任务才能使用 publish_task", []string{"publish_task"}},
		{"同行否定整行排除", "使用 read_file，不要用 write_file", nil},
		{"否定行与正常行并存", "禁止使用 write_file\n但 read_file 和 write_file 都可用", []string{"read_file", "write_file"}},
		{"CRLF 归一化", "使用 read_file\r\n不要调用 write_file", []string{"read_file"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scanPromptToolNames(tc.text)
			sort.Strings(tc.want)
			if len(got) != len(tc.want) {
				t.Fatalf("scanPromptToolNames(%q) = %v，期望 %v", tc.text, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("scanPromptToolNames(%q) = %v，期望 %v", tc.text, got, tc.want)
				}
			}
		})
	}
}

// TestDoctor_PromptMentionsUnauthorizedTool 验证 prompt 提及未授权工具时报 error。
func TestDoctor_PromptMentionsUnauthorizedTool(t *testing.T) {
	dir := t.TempDir()
	promptPath := writePromptFile(t, dir, "worker.md", "使用 read_file 读取，然后 write_file 写入")
	cfg := DefaultConfig()
	cfg.ToolProfiles = map[string][]string{"ro": {"read_file"}}
	cfg.Agents = []AgentKind{{Kind: "worker", Profile: "ro", SystemPromptFile: promptPath}}

	rep := cfg.Doctor()
	if !rep.HasError() {
		t.Fatalf("应检出 error 级诊断: %+v", rep.Diags)
	}
	if !findDiag(rep, DiagError, "worker", "write_file") {
		t.Fatalf("error 诊断应提及 write_file: %+v", rep.Diags)
	}
	if rep.Count(DiagWarning) != 0 || rep.Count(DiagInfo) != 0 {
		t.Fatalf("不应产生 warning/info（profile 已引用且白名单工具均被提及）: %+v", rep.Diags)
	}
}

// TestDoctor_NegationOnlyMentionNoDiag 验证仅在否定语境中提及的未授权工具
// 既不报 error 也不报 info（零诊断）。
func TestDoctor_NegationOnlyMentionNoDiag(t *testing.T) {
	dir := t.TempDir()
	promptPath := writePromptFile(t, dir, "worker.md", "使用 read_file 读取\n禁止使用 write_file 凭空生成报告")
	cfg := DefaultConfig()
	cfg.Agents = []AgentKind{{
		Kind:             "worker",
		Tools:            []string{"read_file"},
		SystemPromptFile: promptPath,
	}}

	rep := cfg.Doctor()
	if len(rep.Diags) != 0 {
		t.Fatalf("仅否定提及未授权工具应零诊断: %+v", rep.Diags)
	}
}

// TestDoctor_ConditionalSentenceStillCounts 验证"只有……才能使用 X"类条件授权句
// 不被否定启发式吞掉：publish_task 未授权仍报 error。
func TestDoctor_ConditionalSentenceStillCounts(t *testing.T) {
	dir := t.TempDir()
	promptPath := writePromptFile(t, dir, "worker.md",
		"使用 read_file 读取\n只有 plan_id 为空且工具实际可用的兼容任务才能使用 publish_task")
	cfg := DefaultConfig()
	cfg.Agents = []AgentKind{{
		Kind:             "worker",
		Tools:            []string{"read_file"},
		SystemPromptFile: promptPath,
	}}

	rep := cfg.Doctor()
	if !rep.HasError() {
		t.Fatalf("条件句中的未授权工具仍应报 error: %+v", rep.Diags)
	}
	if !findDiag(rep, DiagError, "worker", "publish_task") {
		t.Fatalf("error 诊断应提及 publish_task: %+v", rep.Diags)
	}
}

// TestDoctor_AllowlistedToolNotMentioned 验证白名单工具未在 prompt 中提及时报 info。
func TestDoctor_AllowlistedToolNotMentioned(t *testing.T) {
	dir := t.TempDir()
	promptPath := writePromptFile(t, dir, "worker.md", "只使用 read_file 读取文件")
	cfg := DefaultConfig()
	cfg.Agents = []AgentKind{{
		Kind:             "worker",
		Tools:            []string{"read_file", "grep_search"},
		SystemPromptFile: promptPath,
	}}

	rep := cfg.Doctor()
	if rep.HasError() {
		t.Fatalf("不应有 error（prompt 只提及已授权工具）: %+v", rep.Diags)
	}
	if !findDiag(rep, DiagInfo, "worker", "grep_search") {
		t.Fatalf("info 诊断应提及 grep_search: %+v", rep.Diags)
	}
	if findDiag(rep, DiagInfo, "worker", "read_file") {
		t.Fatalf("read_file 已被 prompt 提及，不应报 info: %+v", rep.Diags)
	}
}

// TestDoctor_UnreferencedProfile 验证 tool_profiles 中未被引用的条目报 warning。
func TestDoctor_UnreferencedProfile(t *testing.T) {
	dir := t.TempDir()
	promptPath := writePromptFile(t, dir, "worker.md", "使用 read_file")
	cfg := DefaultConfig()
	cfg.ToolProfiles = map[string][]string{
		"used":   {"read_file"},
		"orphan": {"list_dir"},
	}
	cfg.Agents = []AgentKind{{Kind: "worker", Profile: "used", SystemPromptFile: promptPath}}

	rep := cfg.Doctor()
	if !findDiag(rep, DiagWarning, "", "orphan") {
		t.Fatalf("应对未引用的 orphan profile 报 warning: %+v", rep.Diags)
	}
	if findDiag(rep, DiagWarning, "", "used") {
		t.Fatalf("used profile 已被引用，不应报 warning: %+v", rep.Diags)
	}
}

// TestDoctor_UnreadablePromptFile 验证 prompt 文件不存在时报 warning 且不产生 error。
func TestDoctor_UnreadablePromptFile(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.Agents = []AgentKind{{
		Kind:             "worker",
		Tools:            []string{"read_file"},
		SystemPromptFile: filepath.Join(dir, "missing.md"),
	}}

	rep := cfg.Doctor()
	if rep.HasError() {
		t.Fatalf("prompt 不可读应是 warning 而非 error: %+v", rep.Diags)
	}
	if !findDiag(rep, DiagWarning, "worker", "不可读") && !findDiag(rep, DiagWarning, "worker", "missing.md") {
		t.Fatalf("应对不可读的 prompt 文件报 warning: %+v", rep.Diags)
	}
}

// TestDoctor_CleanConfig 验证完全一致时零诊断。
func TestDoctor_CleanConfig(t *testing.T) {
	dir := t.TempDir()
	promptPath := writePromptFile(t, dir, "worker.md", "使用 read_file 与 grep_search")
	cfg := DefaultConfig()
	cfg.ToolProfiles = map[string][]string{"ro": {"read_file", "grep_search"}}
	cfg.Agents = []AgentKind{{Kind: "worker", Profile: "ro", SystemPromptFile: promptPath}}

	rep := cfg.Doctor()
	if len(rep.Diags) != 0 {
		t.Fatalf("一致配置应零诊断: %+v", rep.Diags)
	}
}

// TestDoctor_LoadValidateDoctorEndToEnd 用临时 yaml 走 LoadConfig + Validate + Doctor
// 完整链路，验证分级计数。yaml 中的 prompt 路径使用 forward slash 绝对路径
// （Validate 的路径风格红线拒绝反斜杠；Windows 上 filepath.ToSlash 转换）。
func TestDoctor_LoadValidateDoctorEndToEnd(t *testing.T) {
	dir := t.TempDir()
	promptPath := writePromptFile(t, dir, "worker.md", "使用 read_file 和 write_file")
	yamlPath := filepath.Join(dir, "setting.yaml")
	yamlContent := `
llm:
  default_model: test-model
tool_profiles:
  worker_ro:
    - read_file
    - grep_search
  orphan_profile:
    - list_dir
agents:
  - kind: worker
    replicas: 1
    profile: worker_ro
    system_prompt_file: ` + filepath.ToSlash(promptPath) + `
    agent_max_loops: 5
    task_max_retries: 2
    enforce_compact_token_threshold: 1000
    context_limit: 4000
`
	if err := os.WriteFile(yamlPath, []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("写入 yaml 失败: %v", err)
	}

	cfg, err := LoadConfig(yamlPath, true)
	if err != nil {
		t.Fatalf("LoadConfig 失败: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate 失败: %v", err)
	}

	rep := cfg.Doctor()
	// prompt 提及 write_file 未授权 → 1 error
	// 白名单 grep_search 未提及 → 1 info
	// orphan_profile 未引用 → 1 warning
	if rep.Count(DiagError) != 1 || rep.Count(DiagWarning) != 1 || rep.Count(DiagInfo) != 1 {
		t.Fatalf("诊断计数 = 错误%d/警告%d/提示%d，期望 1/1/1: %+v",
			rep.Count(DiagError), rep.Count(DiagWarning), rep.Count(DiagInfo), rep.Diags)
	}
}

// TestDoctorCLI_ExitCodes 验证子命令退出码：0=无 error，1=有 error，2=加载/校验失败。
func TestDoctorCLI_ExitCodes(t *testing.T) {
	dir := t.TempDir()

	// 干净配置：prompt 与白名单完全一致
	cleanPrompt := writePromptFile(t, dir, "clean.md", "使用 read_file")
	cleanYAML := filepath.Join(dir, "clean.yaml")
	writeYAML := func(path, promptRef string) {
		t.Helper()
		content := `
llm:
  default_model: test-model
agents:
  - kind: worker
    replicas: 1
    tools: [read_file]
    system_prompt_file: ` + promptRef + `
    agent_max_loops: 5
    task_max_retries: 2
    enforce_compact_token_threshold: 1000
    context_limit: 4000
`
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("写入 yaml 失败: %v", err)
		}
	}
	writeYAML(cleanYAML, filepath.ToSlash(cleanPrompt))

	var out, errOut bytes.Buffer
	if code := doctorCLI([]string{"-config", cleanYAML}, &out, &errOut); code != 0 {
		t.Fatalf("干净配置应退出 0，got %d（stderr: %s）", code, errOut.String())
	}
	if !strings.Contains(out.String(), "汇总：错误 0") {
		t.Fatalf("输出应含汇总行: %q", out.String())
	}

	// 含 error 的配置：prompt 提及 write_file 但未授权
	badPrompt := writePromptFile(t, dir, "bad.md", "使用 write_file 落盘")
	badYAML := filepath.Join(dir, "bad.yaml")
	writeYAML(badYAML, filepath.ToSlash(badPrompt))
	out.Reset()
	errOut.Reset()
	if code := doctorCLI([]string{"-config", badYAML}, &out, &errOut); code != 1 {
		t.Fatalf("含 error 的配置应退出 1，got %d（stderr: %s）", code, errOut.String())
	}

	// 显式指定的不存在配置 → 2
	out.Reset()
	errOut.Reset()
	if code := doctorCLI([]string{"-config", filepath.Join(dir, "nope.yaml")}, &out, &errOut); code != 2 {
		t.Fatalf("加载失败应退出 2，got %d", code)
	}

	// Validate 失败的配置（缺模型）→ 2
	invalidYAML := filepath.Join(dir, "invalid.yaml")
	if err := os.WriteFile(invalidYAML, []byte("agents: []\n"), 0o644); err != nil {
		t.Fatalf("写入 yaml 失败: %v", err)
	}
	out.Reset()
	errOut.Reset()
	if code := doctorCLI([]string{"-config", invalidYAML}, &out, &errOut); code != 2 {
		t.Fatalf("校验失败应退出 2，got %d", code)
	}
	if !strings.Contains(errOut.String(), "[错误]") {
		t.Fatalf("校验失败应打印错误到 stderr: %q", errOut.String())
	}
}
