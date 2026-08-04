package eval

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"encoding/json"

	"gopkg.in/yaml.v3"
)

func TestFingerprintEnvironment_RedactsKeys(t *testing.T) {
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "worker.md")
	if err := os.WriteFile(promptFile, []byte("你是执行代理"), 0o644); err != nil {
		t.Fatal(err)
	}
	tpl := filepath.Join(dir, "tpl.yaml")
	content := `llm:
  base_url: https://api.example.com
  api_key: ${SOME_KEY}
  default_model: m1
search_api_key: sk-search-secret
agents:
  - kind: worker
    system_prompt_file: ` + promptFile + `
`
	if err := os.WriteFile(tpl, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	env1, err := FingerprintEnvironment(tpl)
	if err != nil {
		t.Fatalf("指纹失败: %v", err)
	}
	if env1.Model != "m1" {
		t.Errorf("Model = %q，期望 m1", env1.Model)
	}
	if env1.AgentGoCommit == "" {
		t.Errorf("commit 不应为空（非 git 环境降级 unknown）")
	}

	// 换密钥后 config hash 必须不变（换密钥不是换参照系）
	content2 := strings.Replace(content, "${SOME_KEY}", "sk-明文另一个密钥", 1)
	content2 = strings.Replace(content2, "sk-search-secret", "sk-换了", 1)
	if err := os.WriteFile(tpl, []byte(content2), 0o644); err != nil {
		t.Fatal(err)
	}
	env2, err := FingerprintEnvironment(tpl)
	if err != nil {
		t.Fatal(err)
	}
	if env1.ConfigSHA256 != env2.ConfigSHA256 {
		t.Errorf("抹除密钥后 config hash 应稳定: %s vs %s", env1.ConfigSHA256, env2.ConfigSHA256)
	}

	// 改 base_url 后 config hash 必须变化（参照系变了）
	content3 := strings.Replace(content, "https://api.example.com", "https://api.other.com", 1)
	if err := os.WriteFile(tpl, []byte(content3), 0o644); err != nil {
		t.Fatal(err)
	}
	env3, _ := FingerprintEnvironment(tpl)
	if env1.ConfigSHA256 == env3.ConfigSHA256 {
		t.Errorf("base_url 变化应改变 config hash")
	}

	// 改 prompt 文件内容后 prompt hash 必须变化
	if err := os.WriteFile(promptFile, []byte("你是另一个代理"), 0o644); err != nil {
		t.Fatal(err)
	}
	env4, _ := FingerprintEnvironment(tpl)
	if env1.PromptSHA256 == env4.PromptSHA256 {
		t.Errorf("prompt 内容变化应改变 prompt hash")
	}
}

func TestBaselineCompare(t *testing.T) {
	env := Environment{Model: "m1", PromptSHA256: "p", ConfigSHA256: "c", AgentGoCommit: "x"}
	base := &Baseline{
		Environment: env,
		Tasks: map[string]BaselineTask{
			"taskA": {TerminalStatus: "completed", JudgesPassed: true, WallSec: 100, TotalTokens: 1000, Loops: 10, Errors: 0},
			"taskB": {TerminalStatus: "completed", JudgesPassed: true, WallSec: 50, TotalTokens: 500},
		},
	}

	// 环境可比性键不匹配 → not_comparable 告警（V6：不再整体拒绝，列出差异项）
	ncAlerts, err := base.Compare(Environment{Model: "m2"}, nil, 0.3)
	if err != nil {
		t.Fatal(err)
	}
	if len(ncAlerts) != 1 || ncAlerts[0].Level != "not_comparable" || !strings.Contains(ncAlerts[0].Detail, "model") {
		t.Fatalf("环境不匹配应报 not_comparable: %+v", ncAlerts)
	}

	// 全同 → 无告警
	results := []TaskResult{
		{Name: "taskA", Status: StatusPass, Metrics: RunMetrics{TerminalStatus: "completed", WallSec: 100, PromptTokens: 1000, Loops: 10}},
		{Name: "taskB", Status: StatusPass, Metrics: RunMetrics{TerminalStatus: "completed", WallSec: 50, PromptTokens: 500}},
	}
	alerts, err := base.Compare(env, results, 0.3)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("全同应无告警: %+v", alerts)
	}

	// 终态翻转 → hard；token 超带 → soft
	results[0].Metrics.TerminalStatus = "timeout"
	results[0].Status = StatusFail
	results[1].Metrics.PromptTokens = 800 // +60%
	alerts, err = base.Compare(env, results, 0.3)
	if err != nil {
		t.Fatal(err)
	}
	var hard, soft int
	for _, a := range alerts {
		switch a.Level {
		case "hard":
			hard++
		case "soft":
			soft++
		}
	}
	if hard == 0 {
		t.Errorf("终态翻转 + 判据翻坏应产生 hard 告警: %+v", alerts)
	}
	if soft == 0 {
		t.Errorf("token +60%% 应产生 soft 告警: %+v", alerts)
	}
}

func TestRenderRunConfig(t *testing.T) {
	dir := t.TempDir()
	tpl := filepath.Join(dir, "tpl.yaml")
	content := `llm:
  base_url: https://api.example.com
  api_key: ${EVAL_RENDER_TEST_KEY}
  default_model: m1
agents:
  - kind: worker
ui:
  frontends: [tui]
`
	if err := os.WriteFile(tpl, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.yaml")
	err := renderRunConfig(tpl, out, runOverrides{
		ProjectRoot: filepath.Join(dir, "proj"),
		WebListen:   "127.0.0.1:4321",
		WebToken:    "tok-abc",
	})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	data, _ := os.ReadFile(out)
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("渲染产物不是合法 YAML: %v", err)
	}
	// ${VAR} 占位符必须原样保留（子进程启动时才展开）
	llm := doc["llm"].(map[string]any)
	if llm["api_key"] != "${EVAL_RENDER_TEST_KEY}" {
		t.Errorf("api_key 占位符被改写: %v", llm["api_key"])
	}
	if doc["project_root"] == nil || doc["project_root"] == "" {
		t.Errorf("project_root 未覆盖: %v", doc["project_root"])
	}
	ui := doc["ui"].(map[string]any)
	frontends := ui["frontends"].([]any)
	if len(frontends) != 1 || frontends[0] != "web" {
		t.Errorf("frontends 应为 [web]: %v", frontends)
	}
	web := ui["web"].(map[string]any)
	if web["listen"] != "127.0.0.1:4321" || web["token"] != "tok-abc" {
		t.Errorf("web 覆盖不符: %v", web)
	}
	// 渲染产物权限 0600（含 web token）；Windows 无 POSIX 权限位语义，跳过断言
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(out)
		if info.Mode().Perm() != 0o600 {
			t.Errorf("渲染产物权限 = %o，期望 600", info.Mode().Perm())
		}
	}
}

func TestSnapshotQuietAndActivity(t *testing.T) {
	agent := func(state string) struct {
		State string `json:"state"`
	} {
		return struct {
			State string `json:"state"`
		}{state}
	}
	task := func(status string) struct {
		Status string `json:"status"`
	} {
		return struct {
			Status string `json:"status"`
		}{status}
	}

	idle := &pollSnapshot{SessionCallCount: 3}
	idle.Agents = append(idle.Agents, agent("idle"), agent("idle"))
	idle.Tasks = append(idle.Tasks, task("completed"))
	if !snapshotQuiet(idle) || !snapshotActivity(idle) {
		t.Errorf("全 idle + completed + 有调用应为 quiet+activity")
	}

	busy := &pollSnapshot{}
	busy.Agents = append(busy.Agents, agent("processing"))
	if snapshotQuiet(busy) {
		t.Errorf("有 processing 代理不应 quiet")
	}
	if !snapshotActivity(busy) {
		t.Errorf("有 processing 代理应有 activity")
	}

	pendingTask := &pollSnapshot{SessionCallCount: 1}
	pendingTask.Tasks = append(pendingTask.Tasks, task("pending"))
	if snapshotQuiet(pendingTask) {
		t.Errorf("有 pending 任务不应 quiet")
	}

	fresh := &pollSnapshot{}
	if !snapshotQuiet(fresh) {
		t.Errorf("全空应 quiet")
	}
	if snapshotActivity(fresh) {
		t.Errorf("全空不应 activity（防注入间隙误判收敛）")
	}

	withInteraction := &pollSnapshot{SessionCallCount: 1}
	withInteraction.PendingInteractions = append(withInteraction.PendingInteractions, json.RawMessage(`{}`))
	if snapshotQuiet(withInteraction) {
		t.Errorf("有待答交互不应 quiet")
	}
}
