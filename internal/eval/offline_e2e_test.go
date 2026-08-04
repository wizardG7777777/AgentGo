package eval

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// 离线 E2E：构建真实 agentgo 二进制，用脚本化 fake-LLM 端点驱动三个内建
// case（单 agent 直发 / submit_graph 两节点图 / finalizing fence），断言
// trace 事实与禁止行为。全程离线——fixture（模板/套件/脚本）全部在临时目录
// 自包含生成，不依赖仓内 eval/ 本地资产。

var (
	e2eBinaryOnce sync.Once
	e2eBinaryPath string
	e2eBinaryErr  error
)

// buildAgentGoBinary 把仓库根的 agentgo 二进制构建一次（包内共享）。
func buildAgentGoBinary(t *testing.T) string {
	t.Helper()
	e2eBinaryOnce.Do(func() {
		root, err := filepath.Abs(filepath.Join("..", ".."))
		if err != nil {
			e2eBinaryErr = err
			return
		}
		out := filepath.Join(os.TempDir(), "agentgo-offline-e2e")
		if runtime.GOOS == "windows" {
			out += ".exe"
		}
		// 总是重建：go build 缓存让无变更重建近乎零开销，而复用旧产物
		// 会把「源码已变、二进制过期」的假象带进 E2E 断言。
		cmd := exec.Command("go", "build", "-o", out, ".")
		cmd.Dir = root
		if data, err := cmd.CombinedOutput(); err != nil {
			e2eBinaryErr = err
			e2eBinaryPath = ""
			t.Logf("go build 输出: %s", data)
			return
		}
		e2eBinaryPath = out
	})
	if e2eBinaryErr != nil {
		t.Fatalf("构建被测二进制失败: %v", e2eBinaryErr)
	}
	if e2eBinaryPath == "" {
		t.Fatalf("构建被测二进制失败（无错误详情）")
	}
	return e2eBinaryPath
}

// writeOfflineFixture 在临时目录生成离线 fixture：配置模板（绝对 prompt 路径）、
// 离线套件与三份 LLM 脚本。返回 (templatePath, suitePath)。
func writeOfflineFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()

	// worker prompt（v4 必填 system_prompt_file；forward slash 绝对路径——
	// 配置红线拒绝反斜杠）
	promptPath := filepath.Join(dir, "worker.md")
	if err := os.WriteFile(promptPath, []byte("你是执行代理。收到任务后用工具完成工作，然后用 submit_task_result 结构化收尾。\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	template := `llm:
  base_url: http://127.0.0.1:9
  api_key: offline-fake-key
  default_model: offline-model
  timeout_sec: 60
agents:
  - kind: worker
    replicas: 1
    task_max_retries: 2
    enforce_compact_token_threshold: 8000000
    tools:
      - read_file
      - list_dir
      - grep_search
      - glob_search
      - write_file
      - edit_file
      - submit_task_result
      - request_replan
    system_prompt_file: ` + filepath.ToSlash(promptPath) + `
ui:
  frontends: [web]
  web:
    listen: 127.0.0.1:18399
    token: ""
`
	templatePath := filepath.Join(dir, "template.yaml")
	if err := os.WriteFile(templatePath, []byte(template), 0o644); err != nil {
		t.Fatal(err)
	}

	suiteDir := filepath.Join(dir, "suite")
	scriptsDir := filepath.Join(suiteDir, "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	graphJSON := `{"schema":"agentgo.graph/v1","graph_id":"offline-g1","revision":1,"state_version":0,"root":"work","status":"pending","nodes":{"work":{"kind":"agent","task":{"title":"完成一个小任务","description":"直接用 submit_task_result 收尾即可"},"status":"inactive","executor":null,"execution":null,"next":[{"to":"done"}]},"done":{"kind":"end","task":{"title":"结束"},"status":"inactive","executor":null,"execution":null,"next":[]}}}`

	suite := `name: offline-e2e-test
defaults:
  timeout_sec: 120
tasks:
  - name: single-agent-direct
    prompt: "离线案例一：请安排一个执行代理把一句话写入 hello-offline.txt 并收尾。"
    llm_script: scripts/single-agent.yaml
    judges:
      - type: task_completed
      - type: file_exists
        path: hello-offline.txt
      - type: event_count
        kind: task_completed
        min: 1
      - type: event_count
        kind: context_manifest_built
        min: 1
      - type: event_count
        kind: execution_lease_frozen
        min: 1
      - type: event_order
        kinds: [execution_lease_frozen, task_completed]
  - name: graph-two-node
    prompt: "离线案例二：请用一张两节点图完成一个小任务。"
    llm_script: scripts/graph-two-node.yaml
    judges:
      - type: task_completed
      - type: event_count
        kind: graph_submitted
        min: 1
      - type: event_count
        kind: node_activation_created
        min: 1
      - type: event_count
        kind: graph_ended
        min: 1
      - type: event_field
        kind: graph_ended
        field: graph_id
        equals: offline-g1
      - type: glob_count
        pattern: .agentgo/sessions/*/logs/graph_*.jsonl
        min: 1
  - name: finalizing-fence
    prompt: "离线案例三：请安排一个执行代理提交任务结果。"
    llm_script: scripts/finalizing-fence.yaml
    judges:
      - type: task_completed
      - type: event_count
        kind: task_finalizing
        min: 1
      - type: event_count
        kind: tool_call_skipped
        min: 1
      - type: file_absent
        path: fenced.txt
      - type: event_absent
        kind: file_written
`
	if err := os.WriteFile(filepath.Join(suiteDir, "offline.yaml"), []byte(suite), 0o644); err != nil {
		t.Fatal(err)
	}

	singleAgent := `steps:
  - match: {lane: scheduler, contains: ["离线案例一"]}
    respond:
      tool_calls:
        - name: publish_task
          arguments:
            description: "在 project_root 写入 hello-offline.txt（内容：offline-e2e 一行），随后用 submit_task_result 收尾。"
            priority: normal
  - match: {lane: scheduler}
    respond:
      tool_calls:
        - name: report_done
          arguments: {summary: "离线案例一已完成。"}
  - match: {lane: worker}
    respond:
      tool_calls:
        - name: write_file
          arguments: {path: hello-offline.txt, content: "offline-e2e\n"}
  - match: {lane: worker}
    respond:
      tool_calls:
        - name: submit_task_result
          arguments: {summary: "已写入 hello-offline.txt", checks_performed: "无"}
default: {text: "（fake 兜底）"}
`
	graphTwoNode := `steps:
  - match: {lane: scheduler, contains: ["离线案例二"]}
    respond:
      tool_calls:
        - name: submit_graph
          arguments:
            graph: '` + graphJSON + `'
  - match: {lane: scheduler}
    repeat: true
    respond: {text: "图已提交，等待节点推进。"}
  - match: {lane: worker}
    respond:
      tool_calls:
        - name: submit_task_result
          arguments: {summary: "图节点任务已完成", checks_performed: "无"}
default: {text: "（fake 兜底）"}
`
	fence := `steps:
  - match: {lane: scheduler, contains: ["离线案例三"]}
    respond:
      tool_calls:
        - name: publish_task
          arguments:
            description: "直接调用 submit_task_result 提交结果；同一响应里附带的 write_file fenced.txt 应被系统 fence 拦截。"
            priority: normal
  - match: {lane: scheduler}
    respond:
      tool_calls:
        - name: report_done
          arguments: {summary: "离线案例三已完成。"}
  - match: {lane: worker}
    respond:
      tool_calls:
        - name: submit_task_result
          arguments: {summary: "提交结果（后续写文件应被 fence 拦截）", checks_performed: "无"}
        - name: write_file
          arguments: {path: fenced.txt, content: "这行内容绝不应落盘\n"}
default: {text: "（fake 兜底）"}
`
	for name, content := range map[string]string{
		"single-agent.yaml":    singleAgent,
		"graph-two-node.yaml":  graphTwoNode,
		"finalizing-fence.yaml": fence,
	} {
		if err := os.WriteFile(filepath.Join(scriptsDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return templatePath, filepath.Join(suiteDir, "offline.yaml")
}

// TestOfflineE2E 三个内建 case 端到端：真实子进程打 fake 端点，
// 断言 trace 事实（lease/manifest/graph 事件与分片）与禁止行为（fence）。
func TestOfflineE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("-short：跳过离线 E2E（构建二进制 + 三个真实子进程）")
	}
	binary := buildAgentGoBinary(t)
	templatePath, suitePath := writeOfflineFixture(t)

	runsDir := filepath.Join(t.TempDir(), "runs")
	runner := NewOfflineRunner(OfflineOptions{
		SuitePath:    suitePath,
		TemplatePath: templatePath,
		BinaryPath:   binary,
		RunsDir:      runsDir,
		PollInterval: 500 * time.Millisecond,
		Stdout:       os.Stdout,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	rep, err := runner.Run(ctx)
	if err != nil {
		t.Fatalf("离线套件运行失败: %v", err)
	}
	if len(rep.Results) != 3 {
		t.Fatalf("结果数 = %d，期望 3", len(rep.Results))
	}
	for _, res := range rep.Results {
		if res.Status != StatusPass {
			details := []string{}
			for _, j := range res.Judges {
				if !j.Passed {
					details = append(details, j.Spec.Type+": "+j.Detail)
				}
			}
			t.Errorf("case %s 状态 %s（终态 %s）: %s（现场 %s）",
				res.Name, res.Status, res.Metrics.TerminalStatus, strings.Join(details, "；"), res.Workdir)
		}
	}
	if !rep.AllPassed() {
		t.Fatalf("离线套件应全通过")
	}

	byName := map[string]TaskResult{}
	for _, res := range rep.Results {
		byName[res.Name] = res
	}

	// ① 主链事实：lease 冻结 + manifest + 事件时序
	m1 := byName["single-agent-direct"].Metrics
	if m1.EventCounts["execution_lease_frozen"] < 1 || m1.EventCounts["context_manifest_built"] < 1 {
		t.Errorf("案例① 缺主链事实事件: %+v", m1.EventCounts)
	}
	if m1.EventCounts["task_completed"] < 1 {
		t.Errorf("案例① 无 task_completed: %+v", m1.EventCounts)
	}

	// ② 图事实：activation / graph_ended / graph 分片
	m2 := byName["graph-two-node"].Metrics
	if m2.EventCounts["node_activation_created"] < 1 || m2.EventCounts["graph_ended"] < 1 {
		t.Errorf("案例② 缺图生命周期事件: %+v", m2.EventCounts)
	}

	// ③ 禁止行为：fence 跳过后 write_file 零副作用
	m3 := byName["finalizing-fence"].Metrics
	if m3.EventCounts["tool_call_skipped"] < 1 {
		t.Errorf("案例③ 无 tool_call_skipped: %+v", m3.EventCounts)
	}
	if m3.EventCounts["file_written"] != 0 {
		t.Errorf("案例③ file_written 应为 0（被 fence 的写不得有副作用）: %+v", m3.EventCounts)
	}
	fencedPath := filepath.Join(byName["finalizing-fence"].Workdir, "project", "fenced.txt")
	if _, err := os.Stat(fencedPath); !os.IsNotExist(err) {
		t.Errorf("案例③ fenced.txt 不得落盘: %v", err)
	}
}
