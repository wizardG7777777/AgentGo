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

	"agentgo/internal/graph"
	"agentgo/internal/team"
	"agentgo/internal/trace"
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
    auto_open: false
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

	graphJSON := `{"schema":"agentgo.graph/v1","graph_id":"offline-g1","revision":1,"state_version":0,"root":"work","status":"pending","nodes":{"work":{"kind":"agent","task":{"title":"完成一个小任务","description":"直接用 submit_task_result 收尾即可"},"status":"inactive","executor":null,"execution":null,"next":[{"to":"done","when":{"event":"completed"}}]},"done":{"kind":"end","task":{"title":"结束"},"status":"inactive","executor":null,"execution":null,"next":[]}}}`

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
		"single-agent.yaml":     singleAgent,
		"graph-two-node.yaml":   graphTwoNode,
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

const (
	offlineAcceptanceGraphID  = "offline-acceptance-dataflow-g1"
	offlineAcceptanceArtifact = "acceptance-deliverable.txt"
)

// writeAcceptanceDataflowFixture 构造真实二进制黑盒场景：implement 只写产物，
// 独立 checker 只执行确定性 Shell 检查，二者的结果分别经 implementation 和
// verification 端口交给无 Shell 的 acceptance verifier。verifier 脚本只有在
// 冻结任务描述同时出现两个端口、完整 Result 与各自已解引用 Evidence 时
// 才会提交 pass；任何旁路读取或缺字段都会落入 HTTP 500 兜底并使 case 失败。
func writeAcceptanceDataflowFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	workerPrompt := filepath.Join(dir, "worker.md")
	verifierPrompt := filepath.Join(dir, "verifier.md")
	if err := os.WriteFile(workerPrompt, []byte("你是数据流工作代理；严格在节点能力边界内执行，并用 submit_task_result 提交结构化结果。\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(verifierPrompt, []byte("你是只读验收代理；直接消费任务描述中冻结的 Graph Result/Evidence，禁止 Shell 和旁路图查询。\n"), 0o644); err != nil {
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
    event_type: ""
    tools:
      - write_file
      - run_shell
      - submit_task_result
    system_prompt_file: ` + filepath.ToSlash(workerPrompt) + `
    task_max_retries: 1
    enforce_compact_token_threshold: 8000000
  - kind: verifier
    replicas: 1
    event_type: acceptance.verify
    tools:
      - read_file
      - submit_task_result
    system_prompt_file: ` + filepath.ToSlash(verifierPrompt) + `
    task_max_retries: 1
    enforce_compact_token_threshold: 8000000
infra:
  watchdog:
    interval_sec: 1
    pending_alert_grace_sec: 2
    unroutable_grace_sec: 2
ui:
  frontends: [web]
  web:
    listen: 127.0.0.1:18399
    token: ""
    auto_open: false
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

	graphJSON := `{"schema":"agentgo.graph/v1","graph_id":"` + offlineAcceptanceGraphID + `","revision":1,"state_version":0,"root":"implement","status":"pending","nodes":{"implement":{"kind":"agent","task":{"title":"实现数据流交付物","description":"只写入 ` + offlineAcceptanceArtifact + ` 并提交结构化 implementation Result，不得执行 Shell"},"capability":{"tools":["write_file","submit_task_result"]},"status":"inactive","executor":null,"execution":null,"next":[{"to":"checker","when":{"event":"completed"}},{"to":"verify","target_input":"implementation","when":{"event":"completed"}}]},"checker":{"kind":"agent","task":{"title":"独立检查数据流交付物","description":"只消费 implement 的冻结输入，执行 go version 并提交结构化 verification Result，不得写文件"},"capability":{"tools":["run_shell","submit_task_result"]},"status":"inactive","executor":null,"execution":null,"next":[{"to":"verify","target_input":"verification","when":{"event":"completed"}}]},"verify":{"kind":"acceptance","task":{"title":"只读验收数据流交付物","description":"直接核对 implementation 端口的产物 Result/artifact 证据和 verification 端口的检查 Result/shell 证据；通过时提交 pass","required_inputs":["implementation","verification"],"required_evidence":[{"input":"implementation","kind":"artifact"},{"input":"verification","kind":"shell"}]},"capability":{"tools":["read_file","submit_task_result"]},"status":"inactive","executor":null,"execution":null,"next":[{"to":"done","when":{"path":"$.verdict","operator":"eq","value":"pass"}}]},"done":{"kind":"end","task":{"title":"数据流验收结束"},"status":"inactive","executor":null,"execution":null,"next":[]}}}`

	suite := `name: offline-acceptance-dataflow-e2e
defaults:
  timeout_sec: 60
tasks:
  - name: implement-checker-acceptance-end
    prompt: "数据流验收 E2E：提交实现/检查到独立只读验收再到 end 的图。"
    llm_script: scripts/acceptance-dataflow.yaml
    judges:
      - type: task_completed
      - type: file_exists
        path: ` + offlineAcceptanceArtifact + `
      - type: event_count
        kind: graph_submitted
        min: 1
      - type: event_count
        kind: acceptance_completed
        min: 1
      - type: event_count
        kind: graph_ended
        min: 1
      - type: event_field
        kind: graph_ended
        field: graph_id
        equals: ` + offlineAcceptanceGraphID + `
      - type: event_absent
        kind: task_blocked
      - type: event_absent
        kind: task_failed
`
	if err := os.WriteFile(filepath.Join(suiteDir, "offline.yaml"), []byte(suite), 0o644); err != nil {
		t.Fatal(err)
	}

	script := `steps:
  - match: {lane: scheduler, contains: ["数据流验收 E2E"]}
    respond:
      tool_calls:
        - name: submit_graph
          arguments:
            graph: '` + graphJSON + `'
  - match: {lane: scheduler, contains: ["图已提交并激活", "` + offlineAcceptanceGraphID + `"]}
    respond:
      tool_calls:
        - name: report_done
          arguments: {summary: "验收数据流图已提交，等待图内节点完成。"}
  - match: {lane: worker, contains: ["实现数据流交付物"]}
    respond:
      tool_calls:
        - name: write_file
          arguments: {path: "` + offlineAcceptanceArtifact + `", content: "acceptance-dataflow-e2e\n"}
        - name: submit_task_result
          arguments:
            summary: "实现产物已完成"
            checks_performed: "artifact written"
            evidence: "` + offlineAcceptanceArtifact + `"
            result:
              artifact_path: "` + offlineAcceptanceArtifact + `"
              implementation_ready: true
              metrics: {score: 1}
  - match: {lane: worker, contains: ["独立检查数据流交付物", "来自节点 implement", "完整 Result JSON", "artifact_path", "implementation_ready", "已解引用证据", "[artifact]", "path="]}
    respond:
      tool_calls:
        - name: run_shell
          arguments: {command: "go version"}
  - match: {lane: worker, contains: ["独立检查数据流交付物", "go version"]}
    respond:
      tool_calls:
        - name: submit_task_result
          arguments:
            summary: "独立 checker 已完成"
            checks_performed: "go version"
            result:
              checker_passed: true
              command: "go version"
              metrics: {exit_code: 0}
  - match: {lane: worker, contains: ["只读验收数据流交付物", '"name":"request_replan"']}
    respond:
      error: {status: 500, body: "acceptance verifier must not receive request_replan"}
  - match: {lane: worker, contains: ["只读验收数据流交付物", '"name":"run_shell"']}
    respond:
      error: {status: 500, body: "acceptance verifier must not receive run_shell"}
  - match: {lane: worker, contains: ["只读验收数据流交付物", '"name":"write_file"']}
    respond:
      error: {status: 500, body: "acceptance verifier must not receive write_file"}
  - match: {lane: worker, contains: ["只读验收数据流交付物", '"name":"edit_file"']}
    respond:
      error: {status: 500, body: "acceptance verifier must not receive edit_file"}
  - match: {lane: worker, contains: ["只读验收数据流交付物", '"name":"send_message"']}
    respond:
      error: {status: 500, body: "acceptance verifier must not receive send_message"}
  - match: {lane: worker, contains: ["只读验收数据流交付物", '"name":"publish_task"']}
    respond:
      error: {status: 500, body: "acceptance verifier must not receive publish_task"}
  - match: {lane: worker, contains: ["只读验收数据流交付物", "目标输入端口: implementation", "目标输入端口: verification", "artifact_path", "implementation_ready", "checker_passed", "已解引用证据", "[artifact]", "path=", "[shell]", "command=", "go version", "exit_code=0"]}
    respond:
      tool_calls:
        - name: submit_task_result
          arguments:
            summary: "已直接核对分立冻结的 implementation/verification Result 与证据"
            verdict: pass
            checks_performed: "upstream Result/Evidence"
  - match: {lane: scheduler, contains: ["[graph-ended: ` + offlineAcceptanceGraphID + `/"]}
    respond:
      tool_calls:
        - name: report_done
          arguments: {summary: "数据流验收图已 completed。"}
default:
  error: {status: 500, body: "unexpected acceptance dataflow fake request"}
`
	if err := os.WriteFile(filepath.Join(scriptsDir, "acceptance-dataflow.yaml"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	return templatePath, filepath.Join(suiteDir, "offline.yaml")
}

// TestOfflineE2E_AcceptanceConsumesFrozenDataflow 是 implement → checker →
// acceptance → end（implement 同时 fan-out 直达 acceptance）的真黑盒回归。
// fake 的只有 LLM 端点；二进制、Scheduler、
// Graph Runtime、Runner、证据组装和恢复 Store 均走生产装配。
func TestOfflineE2E_AcceptanceConsumesFrozenDataflow(t *testing.T) {
	if testing.Short() {
		t.Skip("-short：跳过 acceptance 数据流离线 E2E（真实子进程）")
	}
	binary := buildAgentGoBinary(t)
	templatePath, suitePath := writeAcceptanceDataflowFixture(t)
	runner := NewOfflineRunner(OfflineOptions{
		SuitePath:    suitePath,
		TemplatePath: templatePath,
		BinaryPath:   binary,
		RunsDir:      filepath.Join(t.TempDir(), "runs"),
		PollInterval: 500 * time.Millisecond,
		Stdout:       os.Stdout,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	rep, err := runner.Run(ctx)
	if err != nil {
		t.Fatalf("acceptance 数据流离线套件运行失败: %v", err)
	}
	if len(rep.Results) != 1 {
		t.Fatalf("结果数 = %d，期望 1", len(rep.Results))
	}
	res := rep.Results[0]
	if res.Status != StatusPass {
		var details []string
		for _, judge := range res.Judges {
			if !judge.Passed {
				details = append(details, judge.Spec.Type+": "+judge.Detail)
			}
		}
		t.Fatalf("case 状态 %s（终态 %s）: %s（现场 %s）",
			res.Status, res.Metrics.TerminalStatus, strings.Join(details, "；"), res.Workdir)
	}

	projectRoot := filepath.Join(res.Workdir, "project")
	graphStore, err := graph.NewStore(filepath.Join(projectRoot, ".agentgo", "state", "graphs"))
	if err != nil {
		t.Fatalf("打开 GraphStore: %v", err)
	}
	t.Cleanup(func() { _ = graphStore.Close() })
	if err := graphStore.Recover(); err != nil {
		t.Fatalf("恢复 GraphStore: %v", err)
	}
	doc, ok := graphStore.Get(offlineAcceptanceGraphID)
	if !ok {
		t.Fatalf("GraphStore 缺少图 %s", offlineAcceptanceGraphID)
	}
	if doc.Status != graph.GraphCompleted {
		t.Fatalf("图终态 = %s，期望 completed", doc.Status)
	}
	implement := doc.Nodes["implement"]
	checker := doc.Nodes["checker"]
	verify := doc.Nodes["verify"]
	done := doc.Nodes["done"]
	if implement.Execution == nil || implement.Execution.TaskID == "" || implement.Status != graph.NodeCompleted {
		t.Fatalf("implement 缺少 completed execution: %+v", implement)
	}
	if checker.Execution == nil || checker.Execution.TaskID == "" || checker.Status != graph.NodeCompleted {
		t.Fatalf("checker 缺少 completed execution: %+v", checker)
	}
	if verify.Execution == nil || verify.Execution.TaskID == "" || verify.Status != graph.NodeCompleted {
		t.Fatalf("verify 缺少 completed execution: %+v", verify)
	}
	if done.Status != graph.NodeCompleted {
		t.Fatalf("end 节点终态 = %s，期望 completed", done.Status)
	}

	assertOnlyTools := func(label string, capability *graph.Capability, want ...string) {
		t.Helper()
		if capability == nil || len(capability.Tools) != len(want) {
			t.Fatalf("%s capability.tools = %+v，期望 %+v", label, capability, want)
		}
		for i := range want {
			if capability.Tools[i] != want[i] {
				t.Fatalf("%s capability.tools = %+v，期望 %+v", label, capability.Tools, want)
			}
		}
	}
	assertOnlyTools("implement", implement.Capability, "write_file", "submit_task_result")
	assertOnlyTools("checker", checker.Capability, "run_shell", "submit_task_result")
	assertOnlyTools("verifier", verify.Capability, "read_file", "submit_task_result")
	if implement.Execution.Definition == nil || checker.Execution.Definition == nil || verify.Execution.Definition == nil {
		t.Fatalf("执行缺少冻结节点定义: implement=%+v checker=%+v verify=%+v",
			implement.Execution.Definition, checker.Execution.Definition, verify.Execution.Definition)
	}
	assertOnlyTools("implement 冻结定义", implement.Execution.Definition.Capability, "write_file", "submit_task_result")
	assertOnlyTools("checker 冻结定义", checker.Execution.Definition.Capability, "run_shell", "submit_task_result")
	assertOnlyTools("verifier 冻结定义", verify.Execution.Definition.Capability, "read_file", "submit_task_result")
	if len(implement.Next) != 2 || implement.Next[0].To != "checker" || implement.Next[0].TargetInput != "" ||
		implement.Next[1].To != "verify" || implement.Next[1].TargetInput != "implementation" {
		t.Fatalf("implement 必须显式 fan-out 到单入边 checker 和 verify.implementation: %+v", implement.Next)
	}
	if len(checker.Next) != 1 || checker.Next[0].To != "verify" || checker.Next[0].TargetInput != "verification" {
		t.Fatalf("checker 必须只生产 verify.verification: %+v", checker.Next)
	}
	if verify.Task == nil || len(verify.Task.RequiredInputs) != 2 ||
		verify.Task.RequiredInputs[0] != "implementation" || verify.Task.RequiredInputs[1] != "verification" {
		t.Fatalf("acceptance required_inputs 未冻结两个单赋值端口: %+v", verify.Task)
	}

	if len(checker.Execution.Input) != 1 {
		t.Fatalf("checker 输入绑定数 = %d，期望 1: %+v", len(checker.Execution.Input), checker.Execution.Input)
	}
	checkerInput := checker.Execution.Input[0]
	if checkerInput.SourceNodeID != "implement" || checkerInput.TargetInput != "" {
		t.Fatalf("checker 的唯一 implement 输入绑定错误: %+v", checkerInput)
	}
	if len(verify.Execution.Input) != 2 {
		t.Fatalf("verify 输入绑定数 = %d，期望 2: %+v", len(verify.Execution.Input), verify.Execution.Input)
	}
	verifyInputs := make(map[string]graph.InputBinding, len(verify.Execution.Input))
	for _, input := range verify.Execution.Input {
		if _, exists := verifyInputs[input.TargetInput]; exists {
			t.Fatalf("verify 输入端口重复赋值 %q: %+v", input.TargetInput, verify.Execution.Input)
		}
		verifyInputs[input.TargetInput] = input
	}
	implementation, implementationOK := verifyInputs["implementation"]
	verification, verificationOK := verifyInputs["verification"]
	if !implementationOK || !verificationOK {
		t.Fatalf("verify 缺少 implementation/verification 端口: %+v", verify.Execution.Input)
	}
	if implementation.SourceNodeID != "implement" || implementation.SourceActivationID != implement.Execution.ActivationID {
		t.Fatalf("验收 implementation 必须直接来自 implement activation: %+v", implementation)
	}
	if verification.SourceNodeID != "checker" || verification.SourceActivationID != checker.Execution.ActivationID {
		t.Fatalf("验收 verification 必须来自 checker activation: %+v", verification)
	}
	if checkerInput.ResultRef != implementation.ResultRef || string(checkerInput.Result) != string(implementation.Result) {
		t.Fatalf("implement fan-out 未冻结同一份 Result: checker=%+v verifier=%+v", checkerInput, implementation)
	}
	if implementation.ResultRef == "" || !strings.HasPrefix(implementation.ResultRef, "graph-result:"+offlineAcceptanceGraphID+":implement@") {
		t.Fatalf("implementation 缺少稳定 activation ResultRef: %q", implementation.ResultRef)
	}
	if implementation.Truncated || !strings.Contains(string(implementation.Result), `"artifact_path":"`+offlineAcceptanceArtifact+`"`) ||
		!strings.Contains(string(implementation.Result), `"implementation_ready":true`) ||
		!strings.Contains(string(implementation.Result), `"metrics":{"score":1}`) {
		t.Fatalf("implementation 未类型保真携带完整 Result: truncated=%v result=%s", implementation.Truncated, implementation.Result)
	}
	if verification.ResultRef == "" || !strings.HasPrefix(verification.ResultRef, "graph-result:"+offlineAcceptanceGraphID+":checker@") {
		t.Fatalf("verification 缺少稳定 checker ResultRef: %q", verification.ResultRef)
	}
	if verification.Truncated || !strings.Contains(string(verification.Result), `"checker_passed":true`) ||
		!strings.Contains(string(verification.Result), `"command":"go version"`) ||
		!strings.Contains(string(verification.Result), `"metrics":{"exit_code":0}`) {
		t.Fatalf("verification 未类型保真携带完整 Result: truncated=%v result=%s", verification.Truncated, verification.Result)
	}
	storedImplementation, ok := graphStore.ResolveActivationResult(offlineAcceptanceGraphID, implementation.ResultRef)
	if !ok || storedImplementation.NodeID != "implement" || storedImplementation.ActivationID != implement.Execution.ActivationID ||
		string(storedImplementation.Result) != string(implementation.Result) {
		t.Fatalf("ResultRef 无法精确解引用到 implement activation: ok=%v stored=%+v input=%s", ok, storedImplementation, implementation.Result)
	}
	storedVerification, ok := graphStore.ResolveActivationResult(offlineAcceptanceGraphID, verification.ResultRef)
	if !ok || storedVerification.NodeID != "checker" || storedVerification.ActivationID != checker.Execution.ActivationID ||
		string(storedVerification.Result) != string(verification.Result) {
		t.Fatalf("ResultRef 无法精确解引用到 checker activation: ok=%v stored=%+v input=%s", ok, storedVerification, verification.Result)
	}

	findEvidence := func(input graph.InputBinding, kind string) *graph.EvidenceEntry {
		for i := range input.Evidence {
			if input.Evidence[i].Kind == kind {
				return &input.Evidence[i]
			}
		}
		return nil
	}
	for _, evidence := range implementation.Evidence {
		if evidence.Ref == "" || !strings.HasPrefix(evidence.Ref, "ev:"+implement.Execution.TaskID+":") {
			t.Fatalf("implementation EvidenceRef 不是基于 implement 任务身份: %+v", evidence)
		}
		if evidence.Kind == "shell" {
			t.Fatalf("implement 不得产生 shell 证据: %+v", evidence)
		}
	}
	artifactEvidence := findEvidence(implementation, "artifact")
	if artifactEvidence == nil || artifactEvidence.Path != offlineAcceptanceArtifact {
		t.Fatalf("implementation 端口缺少完整 artifact 路径事实: %+v", artifactEvidence)
	}
	checkerArtifactEvidence := findEvidence(checkerInput, "artifact")
	if checkerArtifactEvidence == nil || checkerArtifactEvidence.Ref != artifactEvidence.Ref {
		t.Fatalf("checker 未消费 implement fan-out 的同一 artifact 证据: checker=%+v verify=%+v", checkerArtifactEvidence, artifactEvidence)
	}
	for _, evidence := range verification.Evidence {
		if evidence.Ref == "" {
			t.Fatalf("verification 含空 EvidenceRef: %+v", evidence)
		}
		if strings.HasSuffix(evidence.Ref, ":1") || strings.HasSuffix(evidence.Ref, ":2") {
			t.Fatalf("EvidenceRef 不得使用查询序数: %s", evidence.Ref)
		}
	}
	shellEvidence := findEvidence(verification, "shell")
	if shellEvidence == nil || !strings.HasPrefix(shellEvidence.Ref, "ev:"+checker.Execution.TaskID+":") ||
		shellEvidence.ToolName != "run_shell" || shellEvidence.Command != "go version" ||
		shellEvidence.Success == nil || !*shellEvidence.Success || shellEvidence.ExitCode == nil || *shellEvidence.ExitCode != 0 {
		t.Fatalf("verification 端口缺少结构化 shell 成功事实: %+v", shellEvidence)
	}
	artifactBytes, err := os.ReadFile(filepath.Join(projectRoot, offlineAcceptanceArtifact))
	if err != nil || string(artifactBytes) != "acceptance-dataflow-e2e\n" {
		t.Fatalf("实现产物内容不符合契约: content=%q err=%v", artifactBytes, err)
	}
	implementWrite := false
	implementSubmit := false
	checkerShell := false
	checkerSubmit := false
	verifierSubmit := false
	acceptanceValid := false
	graphEnded := false
	for _, ev := range res.Metrics.TraceEvents {
		if ev.TaskID == implement.Execution.TaskID && ev.Kind == trace.KindToolCall {
			switch ev.Tool {
			case "run_shell":
				t.Fatalf("implement 禁止越界执行 Shell: %+v", ev)
			case "write_file":
				implementWrite = true
			case "submit_task_result":
				implementSubmit = true
			}
		}
		if ev.TaskID == checker.Execution.TaskID && ev.Kind == trace.KindToolCall {
			switch ev.Tool {
			case "write_file", "edit_file":
				t.Fatalf("checker 禁止越界写入: %+v", ev)
			case "run_shell":
				checkerShell = true
			case "submit_task_result":
				checkerSubmit = true
			}
		}
		if ev.TaskID == verify.Execution.TaskID && ev.Kind == trace.KindToolCall {
			switch ev.Tool {
			case "run_shell", "request_replan", "read_graph", "get_task_result", "inspect_task_calls":
				t.Fatalf("verifier 禁止调用 Shell/旁路查询工具 %s: %+v", ev.Tool, ev)
			case "submit_task_result":
				verifierSubmit = true
			}
		}
		if ev.Kind == trace.KindAcceptanceCompleted && ev.GraphID == offlineAcceptanceGraphID &&
			ev.NodeID == "verify" && ev.Acceptance != nil && ev.Acceptance.Status == graph.AcceptValid {
			acceptanceValid = true
		}
		if ev.Kind == trace.KindGraphEnded && ev.GraphID == offlineAcceptanceGraphID {
			graphEnded = true
		}
	}
	if !implementWrite || !implementSubmit || !checkerShell || !checkerSubmit || !verifierSubmit || !acceptanceValid || !graphEnded {
		t.Fatalf("验收因果闭环 trace 不完整: implement_write=%v implement_submit=%v checker_shell=%v checker_submit=%v verifier_submit=%v acceptance_valid=%v graph_ended=%v",
			implementWrite, implementSubmit, checkerShell, checkerSubmit, verifierSubmit, acceptanceValid, graphEnded)
	}
}

const (
	offlineDynamicTeamGraphID        = "offline-dynamic-team-g1"
	offlineDynamicTeamWorkerEvidence = "DYNAMIC-WORKER-EVIDENCE-same-graph-result-readable"
)

// writeDynamicTeamGraphFixture 生成一个刻意不配置静态 agents 的场景：同一
// origin Scheduler task 先 provision 动态 Team，再把其真实 event_type 写进
// Graph。Graph 的 root 是 controller 节点，因此单线程 Scheduler 必须先完成
// origin task，随后才会执行 root 并发布动态 agent 节点。这使历史故障
// 「origin 一终态就拆 Team，图任务随后无人认领」成为确定性时序，而非竞态。
// work 后再进入 summarize controller：worker Result 由 Graph 数据流直接注入
// summarize 的冻结任务描述；汇总节点不得再用 read_graph/get_task_result 旁路
// 回查源任务。
func writeDynamicTeamGraphFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	template := `llm:
  base_url: http://127.0.0.1:9
  api_key: offline-fake-key
  default_model: offline-model
  timeout_sec: 60
infra:
  watchdog:
    interval_sec: 1
    pending_alert_grace_sec: 2
    unroutable_grace_sec: 2
ui:
  frontends: [web]
  web:
    listen: 127.0.0.1:18399
    token: ""
    auto_open: false
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

	graphJSON := `{"schema":"agentgo.graph/v1","graph_id":"` + offlineDynamicTeamGraphID + `","revision":1,"state_version":0,"root":"root","status":"pending","nodes":{"root":{"kind":"controller","task":{"title":"动态 Team 图根控制节点","description":"完成控制节点后推进到动态 Team 执行节点"},"status":"inactive","executor":null,"execution":null,"next":[{"to":"work","when":{"event":"completed"}}]},"work":{"kind":"agent","task":{"title":"动态 Team 执行节点","description":"由刚 provision 的 generalist Team 认领并结构化收尾"},"status":"inactive","executor":null,"execution":null,"metadata":{"route":"{{latest_tool_result.event_type}}"},"next":[{"to":"summarize","when":{"event":"completed"}}]},"summarize":{"kind":"controller","task":{"title":"动态 Team 汇总控制节点","description":"直接消费 Graph 自动注入的 work Result 后汇总；禁止旁路回查源任务"},"status":"inactive","executor":null,"execution":null,"next":[{"to":"done","when":{"event":"completed"}}]},"done":{"kind":"end","task":{"title":"动态 Team 图结束"},"status":"inactive","executor":null,"execution":null,"next":[]}}}`

	suite := `name: offline-dynamic-team-graph-e2e
defaults:
  timeout_sec: 45
tasks:
  - name: dynamic-team-survives-origin
    prompt: "动态 Team 生命周期 E2E：先 provision generalist Team，再用它的真实 event_type 提交图并等待图完成。"
    llm_script: scripts/dynamic-team-graph.yaml
    judges:
      - type: task_completed
      - type: event_count
        kind: graph_submitted
        min: 1
      - type: event_count
        kind: team_graph_bound
        min: 1
      - type: event_count
        kind: graph_ended
        min: 1
      - type: event_count
        kind: team_stopped
        min: 1
      - type: event_field
        kind: graph_ended
        field: graph_id
        equals: ` + offlineDynamicTeamGraphID + `
      - type: event_absent
        kind: task_blocked
      - type: event_absent
        kind: task_failed
`
	if err := os.WriteFile(filepath.Join(suiteDir, "offline.yaml"), []byte(suite), 0o644); err != nil {
		t.Fatal(err)
	}

	script := `steps:
  - match: {lane: scheduler, contains: ["动态 Team 生命周期 E2E"]}
    respond:
      tool_calls:
        - name: provision_agent_team
          arguments:
            template_ref: builtin/generalist@1
            purpose: "offline dynamic Team graph regression"
            graph_id: "` + offlineDynamicTeamGraphID + `"
            replicas: 1
  - match: {lane: scheduler, contains: ["event_type", "offline dynamic Team graph regression"]}
    respond:
      tool_calls:
        - name: submit_graph
          arguments:
            graph: '` + graphJSON + `'
  - match: {lane: scheduler, contains: ["` + offlineDynamicTeamGraphID + `"]}
    respond:
      tool_calls:
        - name: report_done
          arguments: {summary: "origin 已完成 provision + submit_graph，等待图控制器继续。"}
  - match: {lane: scheduler, contains: ["动态 Team 图根控制节点"]}
    respond:
      text: "图根控制节点完成，推进动态 Team 节点。"
  - match: {lane: worker, contains: ["动态 Team 执行节点"]}
    respond:
      tool_calls:
        - name: submit_task_result
          arguments: {summary: "` + offlineDynamicTeamWorkerEvidence + `", checks_performed: "offline trace assertions"}
  - match: {lane: scheduler, contains: ["动态 Team 汇总控制节点", "## 上游输入", "稳定 ResultRef", "完整 Result JSON", "` + offlineDynamicTeamWorkerEvidence + `"]}
    respond:
      text: "已直接消费 Graph 自动注入的同图 worker Result：` + offlineDynamicTeamWorkerEvidence + `"
  - match: {lane: scheduler, contains: ["[graph-ended: ` + offlineDynamicTeamGraphID + `/"]}
    respond:
      tool_calls:
        - name: read_graph
          arguments: {graph_id: "` + offlineDynamicTeamGraphID + `"}
  - match: {lane: scheduler, contains: ["` + offlineDynamicTeamGraphID + `", "completed"]}
    respond:
      tool_calls:
        - name: report_done
          arguments: {summary: "动态 Team 图已核对为 completed。"}
default:
  error: {status: 500, body: "unexpected dynamic Team fake request"}
`
	if err := os.WriteFile(filepath.Join(scriptsDir, "dynamic-team-graph.yaml"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	return templatePath, filepath.Join(suiteDir, "offline.yaml")
}

// TestOfflineE2E_DynamicTeamSurvivesOriginUntilGraphTerminal 是动态 Team ×
// Graph 生命周期的真黑盒回归：真实二进制、真实 Scheduler/Graph/Runner/Store，
// fake 的只有 OpenAI-compatible LLM 端点。断言不依赖 UI 文案，而以 trace、
// GraphStore 和 TeamStore 三份 runtime authority 交叉核对。
func TestOfflineE2E_DynamicTeamSurvivesOriginUntilGraphTerminal(t *testing.T) {
	if testing.Short() {
		t.Skip("-short：跳过动态 Team Graph 离线 E2E（真实子进程）")
	}
	binary := buildAgentGoBinary(t)
	templatePath, suitePath := writeDynamicTeamGraphFixture(t)
	runner := NewOfflineRunner(OfflineOptions{
		SuitePath:    suitePath,
		TemplatePath: templatePath,
		BinaryPath:   binary,
		RunsDir:      filepath.Join(t.TempDir(), "runs"),
		PollInterval: time.Second,
		Stdout:       os.Stdout,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	rep, err := runner.Run(ctx)
	if err != nil {
		t.Fatalf("动态 Team Graph 离线套件运行失败: %v", err)
	}
	if len(rep.Results) != 1 {
		t.Fatalf("结果数 = %d，期望 1", len(rep.Results))
	}
	res := rep.Results[0]
	if res.Status != StatusPass {
		var details []string
		for _, judge := range res.Judges {
			if !judge.Passed {
				details = append(details, judge.Spec.Type+": "+judge.Detail)
			}
		}
		t.Fatalf("case 状态 %s（终态 %s）: %s（现场 %s）",
			res.Status, res.Metrics.TerminalStatus, strings.Join(details, "；"), res.Workdir)
	}

	projectRoot := filepath.Join(res.Workdir, "project")
	graphStore, err := graph.NewStore(filepath.Join(projectRoot, ".agentgo", "state", "graphs"))
	if err != nil {
		t.Fatalf("打开 GraphStore: %v", err)
	}
	t.Cleanup(func() { _ = graphStore.Close() })
	if err := graphStore.Recover(); err != nil {
		t.Fatalf("恢复 GraphStore: %v", err)
	}
	doc, ok := graphStore.Get(offlineDynamicTeamGraphID)
	if !ok {
		t.Fatalf("GraphStore 缺少图 %s", offlineDynamicTeamGraphID)
	}
	if doc.Status != graph.GraphCompleted {
		t.Fatalf("图终态 = %s，期望 completed", doc.Status)
	}
	work, ok := doc.Nodes["work"]
	if !ok || work.Execution == nil || work.Execution.TaskID == "" {
		t.Fatalf("work 节点缺少 durable task execution: %+v", work)
	}
	workTaskID := work.Execution.TaskID
	summarize, ok := doc.Nodes["summarize"]
	if !ok || summarize.Execution == nil || summarize.Execution.TaskID == "" {
		t.Fatalf("summarize 节点缺少 durable task execution: %+v", summarize)
	}
	// 该文本只由 fake 脚本的“冻结任务描述直接含 worker Result”分支产出；
	// 脚本 default 是 HTTP 500，因此不能靠兜底文本假通过。
	if summarize.Status != graph.NodeCompleted ||
		!strings.Contains(summarize.Execution.ResultSummary, offlineDynamicTeamWorkerEvidence) {
		t.Fatalf("summarize 未用 worker 证据完成：status=%s result_ref=%q",
			summarize.Status, summarize.Execution.ResultSummary)
	}
	summarizeTaskID := summarize.Execution.TaskID

	var provision, submitGraph, teamGraphBound, originCompleted trace.Event
	var workClaimed, workCompleted, summarizeClaimed, summarizeCompleted trace.Event
	var graphEnded, teamStopped trace.Event
	for _, ev := range res.Metrics.TraceEvents {
		switch {
		case ev.Kind == trace.KindToolCall && ev.Tool == "provision_agent_team" && provision.TaskID == "":
			provision = ev
		case ev.Kind == trace.KindToolCall && ev.Tool == "submit_graph" && submitGraph.TaskID == "":
			submitGraph = ev
		case ev.Kind == trace.KindTeamGraphBound && ev.GraphID == offlineDynamicTeamGraphID && teamGraphBound.GraphID == "":
			teamGraphBound = ev
		case ev.Kind == trace.KindTaskClaimed && ev.TaskID == workTaskID && workClaimed.TaskID == "":
			workClaimed = ev
		case ev.Kind == trace.KindTaskCompleted && ev.TaskID == workTaskID && workCompleted.TaskID == "":
			workCompleted = ev
		case ev.Kind == trace.KindTaskClaimed && ev.TaskID == summarizeTaskID && summarizeClaimed.TaskID == "":
			summarizeClaimed = ev
		case ev.Kind == trace.KindTaskCompleted && ev.TaskID == summarizeTaskID && summarizeCompleted.TaskID == "":
			summarizeCompleted = ev
		case ev.Kind == trace.KindGraphEnded && ev.GraphID == offlineDynamicTeamGraphID && graphEnded.GraphID == "":
			graphEnded = ev
		case ev.Kind == trace.KindTeamStopped && ev.GraphID == offlineDynamicTeamGraphID && teamStopped.GraphID == "":
			teamStopped = ev
		}
	}
	if provision.TaskID == "" || submitGraph.TaskID == "" {
		t.Fatalf("缺少 provision_agent_team/submit_graph tool_call: provision=%+v submit=%+v", provision, submitGraph)
	}
	if provision.TaskID != submitGraph.TaskID {
		t.Fatalf("provision 与 submit_graph 不在同一 origin task: %s != %s", provision.TaskID, submitGraph.TaskID)
	}
	for _, ev := range res.Metrics.TraceEvents {
		if ev.Kind == trace.KindTaskCompleted && ev.TaskID == provision.TaskID {
			originCompleted = ev
			break
		}
	}
	if teamGraphBound.GraphID == "" || originCompleted.TaskID == "" || workClaimed.TaskID == "" ||
		workCompleted.TaskID == "" || summarizeClaimed.TaskID == "" || summarizeCompleted.TaskID == "" ||
		graphEnded.GraphID == "" || teamStopped.GraphID == "" {
		t.Fatalf("关键时序事件不完整: team_bound=%+v origin_completed=%+v work_claimed=%+v work_completed=%+v summarize_claimed=%+v summarize_completed=%+v graph_ended=%+v team_stopped=%+v",
			teamGraphBound, originCompleted, workClaimed, workCompleted, summarizeClaimed,
			summarizeCompleted, graphEnded, teamStopped)
	}
	if teamGraphBound.TaskID != provision.TaskID {
		t.Fatalf("team_graph_bound provenance=%s，期望 origin=%s", teamGraphBound.TaskID, provision.TaskID)
	}
	if !teamGraphBound.Timestamp.Before(originCompleted.Timestamp) {
		t.Fatalf("Team 绑定未早于 origin 终态：bound=%s origin=%s", teamGraphBound.Timestamp, originCompleted.Timestamp)
	}
	if originCompleted.Timestamp.After(workClaimed.Timestamp) || originCompleted.Timestamp.Equal(workClaimed.Timestamp) {
		t.Fatalf("回归场景未成立：dynamic work 在 origin 终态前/同时已认领：origin=%s claim=%s",
			originCompleted.Timestamp, workClaimed.Timestamp)
	}
	if workClaimed.Timestamp.After(workCompleted.Timestamp) {
		t.Fatalf("work 时序错误：claim=%s complete=%s", workClaimed.Timestamp, workCompleted.Timestamp)
	}
	ordered := []struct {
		name string
		ev   trace.Event
	}{
		{"work_completed", workCompleted},
		{"summarize_claimed", summarizeClaimed},
		{"summarize_completed", summarizeCompleted},
		{"graph_ended", graphEnded},
		{"team_stopped", teamStopped},
	}
	for i := 1; i < len(ordered); i++ {
		if !ordered[i-1].ev.Timestamp.Before(ordered[i].ev.Timestamp) {
			t.Fatalf("Graph 结果读取/收官时序错误：%s=%s 不早于 %s=%s",
				ordered[i-1].name, ordered[i-1].ev.Timestamp,
				ordered[i].name, ordered[i].ev.Timestamp)
		}
	}
	for _, ev := range res.Metrics.TraceEvents {
		if ev.TaskID == summarizeTaskID && ev.Kind == trace.KindToolCall &&
			(ev.Tool == "read_graph" || ev.Tool == "get_task_result" || ev.Tool == "inspect_task_calls") {
			t.Fatalf("summarize 必须直接消费冻结输入，禁止旁路工具 %s: %+v", ev.Tool, ev)
		}
		if (ev.TaskID == workTaskID || ev.TaskID == summarizeTaskID) &&
			(ev.Kind == trace.KindTaskBlocked || ev.Kind == trace.KindTaskFailed || ev.Kind == trace.KindTaskCancelled) {
			t.Fatalf("动态 work/summarize task 出现非成功终态：%+v", ev)
		}
	}

	teamPaths, err := filepath.Glob(filepath.Join(projectRoot, ".agentgo", "sessions", "*", "agent-teams.json"))
	if err != nil || len(teamPaths) != 1 {
		t.Fatalf("定位 TeamStore：paths=%v err=%v", teamPaths, err)
	}
	teamStore, err := team.OpenStore(teamPaths[0])
	if err != nil {
		t.Fatalf("打开 TeamStore: %v", err)
	}
	teams, err := teamStore.List()
	if err != nil || len(teams) != 1 {
		t.Fatalf("TeamStore 记录：teams=%+v err=%v", teams, err)
	}
	spec := teams[0]
	if workClaimed.AgentID == "" || !strings.HasPrefix(workClaimed.AgentID, "generalist-team-"+spec.ID+"-") {
		t.Fatalf("work task 未由 provision 的 generalist Team 认领：agent=%q team=%s", workClaimed.AgentID, spec.ID)
	}
	if spec.Status != team.StatusStopped || spec.StopReason != "graph_terminal:completed" {
		t.Fatalf("Team 最终生命周期 = status %s reason %q，期望 stopped/graph_terminal:completed", spec.Status, spec.StopReason)
	}
	if spec.GraphID != offlineDynamicTeamGraphID {
		t.Fatalf("Team 未 durable 绑定目标 Graph：graph_id=%q want=%q", spec.GraphID, offlineDynamicTeamGraphID)
	}
	if spec.UpdatedAt.Before(graphEnded.Timestamp) {
		t.Fatalf("Team 在 graph_ended 前已停止：team_updated=%s graph_ended=%s", spec.UpdatedAt, graphEnded.Timestamp)
	}
	if spec.UpdatedAt.After(teamStopped.Timestamp) {
		t.Fatalf("team_stopped 事件早于 durable stop：team_updated=%s team_stopped=%s", spec.UpdatedAt, teamStopped.Timestamp)
	}
}
