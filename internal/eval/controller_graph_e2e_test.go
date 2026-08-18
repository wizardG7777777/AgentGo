package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"agentgo/internal/eval/fakellm"
	"agentgo/internal/graph"
	"agentgo/internal/trace"
	"agentgo/internal/ui"
)

const (
	offlineControllerGraphID       = "offline-controller-end-g1"
	offlineControllerInternalText  = "INTERNAL-CONTROLLER-NODE-RESULT"
	offlineControllerFinalText     = "FINAL-AFTER-GRAPH-ENDED"
	offlineControllerPrematureText = "PREMATURE-ORIGIN-REPORT-DONE"
)

// writeControllerEndFixture 生成最小 controller → end 真实二进制场景。
// origin Scheduler 在同一次 LLM 响应中故意把 report_done 排在
// submit_graph 后面；只有 submit_graph 成功即 finalizing 的 fence 才能让
// 它无副作用跳过。root controller 故意用自然文本完成，用于证明
// 该文本只结算节点，不会进入用户 ResultOutput/ResultSnapshot。
func writeControllerEndFixture(t *testing.T) (templatePath, scriptPath string) {
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
	templatePath = filepath.Join(dir, "template.yaml")
	if err := os.WriteFile(templatePath, []byte(template), 0o644); err != nil {
		t.Fatal(err)
	}

	graphJSON := `{"schema":"agentgo.graph/v1","graph_id":"` + offlineControllerGraphID + `","revision":1,"state_version":0,"root":"root","status":"pending","nodes":{"root":{"kind":"controller","task":{"title":"最小 Controller 节点","description":"以自然文本完成当前节点，由 Runtime 推进 end"},"capability":{"tools":["read_graph"]},"status":"inactive","executor":null,"execution":null,"next":[{"to":"done","when":{"event":"completed"}}]},"done":{"kind":"end","task":{"title":"最小图收官"},"status":"inactive","executor":null,"execution":null,"next":[]}}}`
	script := `steps:
  - match: {lane: scheduler, contains: ["最小 controller Graph E2E"]}
    respond:
      tool_calls:
        - name: submit_graph
          arguments:
            graph: '` + graphJSON + `'
        - name: report_done
          arguments: {summary: "` + offlineControllerPrematureText + `"}
  - match: {lane: scheduler, contains: ["[graph-ended: ` + offlineControllerGraphID + `/"]}
    respond: {text: "` + offlineControllerFinalText + `"}
  - match: {lane: worker, contains: ["最小 Controller 节点"]}
    respond: {text: "` + offlineControllerInternalText + `"}
default:
  error: {status: 500, body: "unexpected minimal controller graph fake request"}
`
	scriptPath = filepath.Join(dir, "controller-end.yaml")
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	return templatePath, scriptPath
}

func fetchControllerSnapshot(ctx context.Context, client *http.Client, baseURL, token string) (*ui.Snapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/snapshot", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("snapshot HTTP %d", resp.StatusCode)
	}
	var snap ui.Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

func controllerSnapshotSettled(snap *ui.Snapshot) bool {
	if snap == nil || snap.LastResult == nil || !strings.Contains(snap.LastResult.Text, offlineControllerFinalText) {
		return false
	}
	graphCompleted := false
	for _, view := range snap.Graphs {
		if view.GraphID == offlineControllerGraphID && view.Status == string(graph.GraphCompleted) {
			graphCompleted = true
		}
	}
	if !graphCompleted {
		return false
	}
	for _, task := range snap.Tasks {
		if task.Status == "pending" || task.Status == "processing" {
			return false
		}
	}
	return len(snap.PendingInteractions) == 0
}

func controllerRecordSummary(records []fakellm.RecordedRequest) string {
	var summary strings.Builder
	for i, record := range records {
		if i > 0 {
			summary.WriteString("; ")
		}
		fmt.Fprintf(&summary, "lane=%s step=%d tools=%v", record.Lane, record.Step, record.ToolNames)
	}
	return summary.String()
}

// TestOfflineE2E_MinimalControllerEndHasSinglePostGraphResult 是统一 Graph
// 最小退化形态的真黑盒回归：真实 agentgo 二进制、真实
// Scheduler/Graph Runtime/Store/UI Hub，只有 OpenAI-compatible LLM 是本地脚本。
//
// 它同时钉住四个跨层事实：
//   - submit_graph 成功后 origin 静默交棒，同响应尾随 report_done 被 fence；
//   - controller 节点的 per-node capability 真实收窄 LLM tool definitions；
//   - controller 自然文本只进 Graph Result/轮次审计，不进用户 result feed；
//   - graph_ended 之后的非图 Scheduler 唤醒产生唯一用户最终结果。
func TestOfflineE2E_MinimalControllerEndHasSinglePostGraphResult(t *testing.T) {
	if testing.Short() {
		t.Skip("-short：跳过最小 controller → end 真实二进制 E2E")
	}
	binary := buildAgentGoBinary(t)
	templatePath, scriptPath := writeControllerEndFixture(t)
	script, err := fakellm.LoadScript(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	fake := fakellm.NewServer(script)
	defer fake.Close()

	workdir := t.TempDir()
	projectRoot := filepath.Join(workdir, "project")
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	token := randHex(16)
	configPath := filepath.Join(workdir, "config.yaml")
	if err := renderRunConfig(templatePath, configPath, runOverrides{
		ProjectRoot: projectRoot,
		WebListen:   fmt.Sprintf("127.0.0.1:%d", port),
		WebToken:    token,
		OfflineLLM:  &offlineLLMOverride{BaseURL: fake.URL(), APIKey: "offline-fake-key", Model: "offline-model"},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	childCtx, stopChild := context.WithCancel(ctx)
	logPath := filepath.Join(workdir, "child.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(childCtx, binary, "-config", configPath, "-skip-startup-probe")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatal(err)
	}
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		stopChild()
		_ = cmd.Wait()
		_ = logFile.Close()
	}
	defer stop()

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: 5 * time.Second}
	healthDeadline := time.Now().Add(45 * time.Second)
	healthy := false
	for time.Now().Before(healthDeadline) {
		resp, requestErr := client.Get(baseURL + "/healthz")
		if requestErr == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				healthy = true
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !healthy {
		t.Fatalf("被测二进制未在期限内健康（log=%s）", logPath)
	}
	driver := NewRunner(RunnerOptions{})
	if err := driver.postInput(ctx, baseURL, token, "最小 controller Graph E2E：提交 controller → end 图并只在图终态后回复。"); err != nil {
		t.Fatal(err)
	}

	var finalSnapshot *ui.Snapshot
	var lastSnapshot *ui.Snapshot
	settleDeadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(settleDeadline) {
		snap, fetchErr := fetchControllerSnapshot(ctx, client, baseURL, token)
		if fetchErr == nil {
			lastSnapshot = snap
			if controllerSnapshotSettled(snap) {
				finalSnapshot = snap
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if finalSnapshot == nil {
		stop()
		childLog, _ := os.ReadFile(logPath)
		var lastResult any
		var graphs, tasks any
		if lastSnapshot != nil {
			lastResult = lastSnapshot.LastResult
			graphs = lastSnapshot.Graphs
			tasks = lastSnapshot.Tasks
		}
		t.Fatalf("最小 controller Graph 未在期限内收敛: last_result=%+v graphs=%+v tasks=%+v records=%s log=%s",
			lastResult, graphs, tasks, controllerRecordSummary(fake.Records()), string(childLog))
	}

	var resultOutputs []ui.FeedOutput
	for _, out := range finalSnapshot.Feed.Outputs {
		if out.Kind == "result" {
			resultOutputs = append(resultOutputs, out)
		}
		if strings.Contains(out.Text, offlineControllerInternalText) || strings.Contains(out.Text, offlineControllerPrematureText) {
			t.Fatalf("Graph 终态前的内部文本泄漏进用户 feed: %+v", out)
		}
	}
	if len(resultOutputs) != 1 || !strings.Contains(resultOutputs[0].Text, offlineControllerFinalText) {
		t.Fatalf("用户 result feed=%+v，期望仅一条 graph_ended 后收官结果", resultOutputs)
	}
	if finalSnapshot.LastResult == nil || !strings.Contains(finalSnapshot.LastResult.Text, offlineControllerFinalText) ||
		strings.Contains(finalSnapshot.LastResult.Text, offlineControllerInternalText) {
		t.Fatalf("会话 ResultSnapshot 不符合图终态收官语义: %+v", finalSnapshot.LastResult)
	}

	stop()
	events, incomplete := CollectTraceEventsWithStatus(projectRoot)
	if len(incomplete) != 0 {
		t.Fatalf("trace 证据不完整: %v", incomplete)
	}
	graphStore, err := graph.NewStore(filepath.Join(projectRoot, ".agentgo", "state", "graphs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = graphStore.Close() })
	if err := graphStore.Recover(); err != nil {
		t.Fatal(err)
	}
	doc, ok := graphStore.Get(offlineControllerGraphID)
	if !ok || doc.Status != graph.GraphCompleted {
		t.Fatalf("最小图终态错误: ok=%v doc=%+v", ok, doc)
	}
	root := doc.Nodes["root"]
	if root.Status != graph.NodeCompleted || root.Execution == nil ||
		!strings.Contains(root.Execution.ResultSummary, offlineControllerInternalText) {
		t.Fatalf("controller 自然文本未作为节点 Result 正常结算: %+v", root)
	}

	records := fake.Records()
	var rootTools []string
	for _, record := range records {
		if strings.Contains(string(record.Body), "最小 Controller 节点") &&
			!strings.Contains(string(record.Body), "[graph-ended: "+offlineControllerGraphID+"/") {
			rootTools = append([]string(nil), record.ToolNames...)
		}
	}
	sort.Strings(rootTools)
	wantTools := []string{"read_graph", "request_replan", "submit_task_result"}
	if strings.Join(rootTools, ",") != strings.Join(wantTools, ",") {
		t.Fatalf("Graph controller 的 LLM tool definitions=%v，期望 per-node 租约精确收窄为 %v", rootTools, wantTools)
	}

	var graphEnded trace.Event
	skippedReportDone := false
	for _, ev := range events {
		if ev.Kind == trace.KindGraphEnded && ev.GraphID == offlineControllerGraphID {
			graphEnded = ev
		}
		if ev.Kind == trace.KindToolCallSkipped && ev.Tool == "report_done" && ev.Reason == "task_finalizing" {
			skippedReportDone = true
		}
	}
	if graphEnded.GraphID == "" || !skippedReportDone {
		t.Fatalf("缺少 graph_ended 或 origin report_done finalizing fence 事实: ended=%+v skipped=%v", graphEnded, skippedReportDone)
	}
	if graphEnded.Timestamp.After(resultOutputs[0].At) {
		t.Fatalf("用户结果早于 graph_ended: graph=%s result=%s", graphEnded.Timestamp, resultOutputs[0].At)
	}
}
