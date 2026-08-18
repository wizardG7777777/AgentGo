package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentgo/internal/agent"
	"agentgo/internal/model"
	"agentgo/internal/store"
	"agentgo/internal/trace"
)

// newSubmitGroup 构造一个已注入提交通道的 PlanControlGroup（runner 装配形态）。
func newSubmitGroup(taskStore store.TaskStore, holder TaskHolder) (PlanControlGroup, *fakeFinalizationNotifier, *agent.SubmitState) {
	notifier := &fakeFinalizationNotifier{}
	state := agent.NewSubmitState()
	return PlanControlGroup{
		Store:                taskStore,
		Holder:               holder,
		AgentID:              "worker-1",
		FinalizationNotifier: notifier,
		SubmitState:          state,
	}, notifier, state
}

func publishPlainTask(t *testing.T, s store.TaskStore, task *model.Task) *model.Task {
	t.Helper()
	if err := s.PublishTask(task); err != nil {
		t.Fatal(err)
	}
	return task
}

// submit_task_result 只在 FinalizationNotifier 与 SubmitState 都注入时注册；
// 注册不依赖任何控制面装配（普通 runner 形态也应可用）。
func TestSubmitTaskResultRegisteredOnlyWhenChannelInjected(t *testing.T) {
	registered := func(g PlanControlGroup) bool {
		reg := agent.NewToolRegistry()
		g.Register(reg)
		for _, def := range reg.Defs() {
			if def.Name == "submit_task_result" {
				return true
			}
		}
		return false
	}
	s := store.NewMemoryTaskStore(nil, 8, 1, 60)
	holder := &fakeHolder{id: "t1"}

	if registered(PlanControlGroup{Store: s, Holder: holder}) {
		t.Error("未注入提交通道时不应注册 submit_task_result（不完整装配）")
	}
	if registered(PlanControlGroup{Store: s, Holder: holder, FinalizationNotifier: &fakeFinalizationNotifier{}}) {
		t.Error("仅注入 FinalizationNotifier 时不应注册")
	}
	if registered(PlanControlGroup{Store: s, Holder: holder, SubmitState: agent.NewSubmitState()}) {
		t.Error("仅注入 SubmitState 时不应注册")
	}
	g, _, _ := newSubmitGroup(s, holder)
	if !registered(g) {
		t.Error("两个通道都注入时必须注册")
	}
}

// 成功路径（普通非图任务）：提交暂存 + finalized 标记 + 中文确认文本。
func TestSubmitTaskResultSuccess(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 8, 1, 60)
	task := publishPlainTask(t, s, &model.Task{Description: "write report", EventType: "code"})
	g, notifier, state := newSubmitGroup(s, &fakeHolder{id: task.ID})

	reply, err := g.submitTaskResult(context.Background(), map[string]any{
		"summary":          "报告已写入 report.md",
		"checks_performed": "go build, go test ./...",
		"evidence":         "report.md",
		"remaining_risks":  "覆盖率未测",
		"result": map[string]any{
			"coverage": "gap",
			"metrics":  map[string]any{"score": 0.75, "ready": true},
			"items":    []any{"a", 2},
		},
	})
	if err != nil {
		t.Fatalf("submitTaskResult: %v", err)
	}
	if !notifier.marked {
		t.Error("成功后必须 MarkTaskFinalized")
	}
	if !strings.Contains(reply, "停止调用其他工具") {
		t.Errorf("确认文本应提醒停止调用其他工具，实际：%s", reply)
	}
	sub, ok := state.Take(task.ID)
	if !ok {
		t.Fatal("SubmitState 应暂存本次提交")
	}
	if sub.Summary != "报告已写入 report.md" {
		t.Errorf("Summary = %q", sub.Summary)
	}
	if len(sub.ChecksPerformed) != 2 || sub.ChecksPerformed[0] != "go build" || sub.ChecksPerformed[1] != "go test ./..." {
		t.Errorf("ChecksPerformed 逗号拆分错误: %v", sub.ChecksPerformed)
	}
	if len(sub.Evidence) != 1 || len(sub.RemainingRisks) != 1 {
		t.Errorf("Evidence/RemainingRisks 拆分错误: %v / %v", sub.Evidence, sub.RemainingRisks)
	}
	structured, err := agent.DecodeStructuredResult(sub.ResultJSON)
	if err != nil {
		t.Fatalf("ResultJSON 应为合法 object: %v（raw=%q）", err, sub.ResultJSON)
	}
	if structured["coverage"] != "gap" {
		t.Fatalf("coverage 未类型保真暂存: %+v", structured)
	}
	metrics, ok := structured["metrics"].(map[string]any)
	if !ok || metrics["score"] != float64(0.75) || metrics["ready"] != true {
		t.Fatalf("嵌套 number/bool 未类型保真暂存: %+v", structured)
	}
}

func TestSubmitTaskResultSchemaDeclaresStrictStructuredResult(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 8, 1, 60)
	g, _, _ := newSubmitGroup(s, &fakeHolder{id: "t1"})
	reg := agent.NewToolRegistry()
	g.Register(reg)
	for _, def := range reg.Defs() {
		if def.Name != "submit_task_result" {
			continue
		}
		if allow, ok := def.Parameters["additionalProperties"].(bool); !ok || allow {
			t.Fatalf("submit_task_result additionalProperties=%#v，want false", def.Parameters["additionalProperties"])
		}
		props := def.Parameters["properties"].(map[string]any)
		result := props["result"].(map[string]any)
		if result["type"] != "object" || result["maxProperties"] != structuredResultMaxKeys {
			t.Fatalf("result schema 未声明 object/字段上限: %#v", result)
		}
		if pattern := result["propertyNames"].(map[string]any)["pattern"]; pattern != "^[A-Za-z_][A-Za-z0-9_]{0,63}$" {
			t.Fatalf("result propertyNames pattern=%#v", pattern)
		}
		return
	}
	t.Fatal("未注册 submit_task_result")
}

func TestSubmitTaskResultRejectsInvalidStructuredResult(t *testing.T) {
	tooMany := make(map[string]any, structuredResultMaxKeys+1)
	for i := 0; i <= structuredResultMaxKeys; i++ {
		tooMany[fmt.Sprintf("key_%d", i)] = i
	}
	deep := map[string]any{"leaf": true}
	for i := 0; i < structuredResultMaxDepth; i++ {
		deep = map[string]any{"child": deep}
	}

	cases := []struct {
		name      string
		args      map[string]any
		wantError string
	}{
		{"未知工具参数", map[string]any{"summary": "完成", "covergae": "gap"}, "未知参数"},
		{"result 不是 object", map[string]any{"summary": "完成", "result": []any{"gap"}}, "JSON object"},
		{"result 为 null", map[string]any{"summary": "完成", "result": nil}, "JSON object"},
		{"系统 status 保留键", map[string]any{"summary": "完成", "result": map[string]any{"status": "completed"}}, "系统保留键"},
		{"系统 event 保留键", map[string]any{"summary": "完成", "result": map[string]any{"event": "ready"}}, "系统保留键"},
		{"系统 verdict 保留键", map[string]any{"summary": "完成", "result": map[string]any{"verdict": "pass"}}, "系统保留键"},
		{"系统 evidence 保留键", map[string]any{"summary": "完成", "result": map[string]any{"cited_evidence": "ev:x:1"}}, "系统保留键"},
		{"非法路径键", map[string]any{"summary": "完成", "result": map[string]any{"bad.key": true}}, "字段名"},
		{"字段过多", map[string]any{"summary": "完成", "result": tooMany}, "字段数"},
		{"嵌套过深", map[string]any{"summary": "完成", "result": deep}, "嵌套深度"},
		{"载荷过大", map[string]any{"summary": "完成", "result": map[string]any{"blob": strings.Repeat("x", structuredResultMaxBytes)}}, "大小"},
		{"不支持类型", map[string]any{"summary": "完成", "result": map[string]any{"stream": make(chan int)}}, "不支持"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := store.NewMemoryTaskStore(nil, 8, 1, 60)
			task := publishPlainTask(t, s, &model.Task{Description: "structured", EventType: "code"})
			g, notifier, state := newSubmitGroup(s, &fakeHolder{id: task.ID})
			_, err := g.submitTaskResult(context.Background(), tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error=%v，want 包含 %q", err, tc.wantError)
			}
			if notifier.marked {
				t.Fatal("拒绝的 result 不得进入 finalizing")
			}
			if _, ok := state.Take(task.ID); ok {
				t.Fatal("拒绝的 result 不得写入 SubmitState")
			}
		})
	}
}

func TestSubmitTaskResultRejectsAgentIDCollision(t *testing.T) {
	t.Run("自定义字段与正文键冲突", func(t *testing.T) {
		s := store.NewMemoryTaskStore(nil, 8, 1, 60)
		task := publishPlainTask(t, s, &model.Task{Description: "structured", EventType: "code"})
		g, notifier, _ := newSubmitGroup(s, &fakeHolder{id: task.ID})
		g.AgentID = "worker_1"
		_, err := g.submitTaskResult(context.Background(), map[string]any{
			"summary": "完成",
			"result":  map[string]any{"worker_1": "伪造正文"},
		})
		if err == nil || !strings.Contains(err.Error(), "agent_id") {
			t.Fatalf("agent_id 冲突应被拒绝，实际 err=%v", err)
		}
		if notifier.marked {
			t.Fatal("冲突 result 不得进入 finalizing")
		}
	})

	for _, agentID := range []string{"status", "event", "verdict", "cited_evidence", agent.StructuredResultStorageKey} {
		t.Run("正文键占用系统协议_"+agentID, func(t *testing.T) {
			s := store.NewMemoryTaskStore(nil, 8, 1, 60)
			task := publishPlainTask(t, s, &model.Task{Description: "structured", EventType: "code"})
			g, notifier, _ := newSubmitGroup(s, &fakeHolder{id: task.ID})
			g.AgentID = agentID
			_, err := g.submitTaskResult(context.Background(), map[string]any{"summary": "完成"})
			if err == nil || !strings.Contains(err.Error(), "系统结果键冲突") {
				t.Fatalf("agent_id=%q 应 fail-closed，实际 err=%v", agentID, err)
			}
			if notifier.marked {
				t.Fatal("冲突 agent_id 不得进入 finalizing")
			}
			got, getErr := s.GetTask(task.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if got.Status != task.Status || len(got.Results) != 0 {
				t.Fatalf("拒绝后不得遗留终态或结果: status=%s results=%v", got.Status, got.Results)
			}
		})
	}
}

// 拒绝对象（C6b）：非图 __scheduler__ 任务（指引用 report_done）。Graph
// controller 同样路由到 __scheduler__，但必须可以提交 event / 自定义结果；
// acceptance 则通过同一工具提交 verdict。
func TestSubmitTaskResultRejectsSchedulerTask(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 8, 1, 60)
	task := publishPlainTask(t, s, &model.Task{Description: "sched", EventType: "__scheduler__"})
	g, notifier, state := newSubmitGroup(s, &fakeHolder{id: task.ID})
	_, err := g.submitTaskResult(context.Background(), map[string]any{"summary": "done"})
	if err == nil || !strings.Contains(err.Error(), "report_done") {
		t.Fatalf("scheduler 任务应被拒绝并指引 report_done，实际 err=%v", err)
	}
	if notifier.marked {
		t.Error("拒绝时不应 MarkTaskFinalized")
	}
	if _, ok := state.Take(task.ID); ok {
		t.Error("拒绝时不应暂存提交")
	}

	// 普通 Graph agent 节点：正常接受；验收节点另走 verdict-only 契约。
	graphTask := publishPlainTask(t, s, &model.Task{
		Description: "graph node", EventType: "code",
		GraphID: "g-1", NodeID: "verify", ActivationID: "verify@1",
	})
	g2, notifier2, _ := newSubmitGroup(s, &fakeHolder{id: graphTask.ID})
	if _, err := g2.submitTaskResult(context.Background(), map[string]any{"summary": "验收完成", "verdict": "pass"}); err != nil {
		t.Fatalf("图节点任务应可正常提交: %v", err)
	}
	if !notifier2.marked {
		t.Error("图节点任务成功后必须 MarkTaskFinalized")
	}

	// Graph controller：EventType 仍是 __scheduler__，GraphID 才是角色判定
	// 的权威边界；必须接受并保留事件，供 Runtime 推进条件边。
	controllerTask := publishPlainTask(t, s, &model.Task{
		Description: "graph controller", EventType: "__scheduler__",
		GraphID: "g-1", NodeID: "summarize", ActivationID: "summarize@1",
	})
	g3, notifier3, state3 := newSubmitGroup(s, &fakeHolder{id: controllerTask.ID})
	if _, err := g3.submitTaskResult(context.Background(), map[string]any{
		"summary": "覆盖度已裁决", "event": "ready",
	}); err != nil {
		t.Fatalf("Graph controller 应可提交结构化结果: %v", err)
	}
	if !notifier3.marked {
		t.Error("Graph controller 成功后必须 MarkTaskFinalized")
	}
	sub, ok := state3.Take(controllerTask.ID)
	if !ok || sub.Event != "ready" {
		t.Fatalf("Graph controller event 未进入 SubmitState: %+v, ok=%t", sub, ok)
	}
}

// event 参数（C5b）：随结构化提交暂存并去空白，供 agent 收尾时写入
// task.Results["event"] 驱动 Graph 事件形态转移条件。
func TestSubmitTaskResultEventParam(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 8, 1, 60)
	task := publishPlainTask(t, s, &model.Task{Description: "graph node", EventType: "code", GraphID: "g-1"})
	g, _, state := newSubmitGroup(s, &fakeHolder{id: task.ID})
	if _, err := g.submitTaskResult(context.Background(), map[string]any{
		"summary": "材料已就绪", "event": " ready ",
	}); err != nil {
		t.Fatalf("submitTaskResult: %v", err)
	}
	sub, ok := state.Take(task.ID)
	if !ok {
		t.Fatal("SubmitState 应暂存本次提交")
	}
	if sub.Event != "ready" {
		t.Errorf("Event = %q，应去空白为 ready", sub.Event)
	}

	// 省略 event：零值暂存，不写 Results["event"]（agent 收尾路径按空串跳过）。
	task2 := publishPlainTask(t, s, &model.Task{Description: "plain node", EventType: "code"})
	g2, _, state2 := newSubmitGroup(s, &fakeHolder{id: task2.ID})
	if _, err := g2.submitTaskResult(context.Background(), map[string]any{"summary": "完成"}); err != nil {
		t.Fatalf("submitTaskResult: %v", err)
	}
	sub2, _ := state2.Take(task2.ID)
	if sub2 == nil || sub2.Event != "" {
		t.Errorf("未传 event 时 Event 应为零值，实际 %+v", sub2)
	}

	// Graph 条件校验只接受固定事件词表；自定义 event 若被暂存，会令所有
	// 合法边都无法命中并把图误置 failed，因此在 finalizing 前拒绝。
	task3 := publishPlainTask(t, s, &model.Task{Description: "graph node", EventType: "code", GraphID: "g-2"})
	g3, notifier3, state3 := newSubmitGroup(s, &fakeHolder{id: task3.ID})
	if _, err := g3.submitTaskResult(context.Background(), map[string]any{
		"summary": "完成", "event": "custom.done",
	}); err == nil || !strings.Contains(err.Error(), "事件词表") {
		t.Fatalf("自定义 Graph event 应在收尾前拒绝，实际 err=%v", err)
	}
	if notifier3.marked {
		t.Error("非法 event 不得进入 finalizing")
	}
	if _, ok := state3.Take(task3.ID); ok {
		t.Error("非法 event 不得写入 SubmitState")
	}
}

// verdict 参数（C5b）：随结构化提交暂存并去空白，供 agent 收尾时写入
// task.Results["verdict"] 驱动 Graph acceptance 节点的路径边条件。
func TestSubmitTaskResultVerdictParam(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 8, 1, 60)
	task := publishPlainTask(t, s, &model.Task{Description: "acceptance node", EventType: "code", GraphID: "g-1"})
	g, _, state := newSubmitGroup(s, &fakeHolder{id: task.ID})
	if _, err := g.submitTaskResult(context.Background(), map[string]any{
		"summary": "验收完成", "verdict": " fixable ",
	}); err != nil {
		t.Fatalf("submitTaskResult: %v", err)
	}
	sub, ok := state.Take(task.ID)
	if !ok {
		t.Fatal("SubmitState 应暂存本次提交")
	}
	if sub.Verdict != "fixable" {
		t.Errorf("Verdict = %q，应去空白为 fixable", sub.Verdict)
	}

	// 省略 verdict：零值暂存，不写 Results["verdict"]。
	task2 := publishPlainTask(t, s, &model.Task{Description: "plain node", EventType: "code"})
	g2, _, state2 := newSubmitGroup(s, &fakeHolder{id: task2.ID})
	if _, err := g2.submitTaskResult(context.Background(), map[string]any{"summary": "完成"}); err != nil {
		t.Fatalf("submitTaskResult: %v", err)
	}
	sub2, _ := state2.Take(task2.ID)
	if sub2 == nil || sub2.Verdict != "" {
		t.Errorf("未传 verdict 时 Verdict 应为零值，实际 %+v", sub2)
	}
}

func TestSubmitTaskResultVerdictContract(t *testing.T) {
	for _, verdict := range []string{"pass", "fixable", "failed"} {
		t.Run("允许_"+verdict, func(t *testing.T) {
			s := store.NewMemoryTaskStore(nil, 8, 1, 60)
			task := publishPlainTask(t, s, &model.Task{Description: "acceptance node", EventType: "acceptance.verify", GraphID: "g-1"})
			g, _, _ := newSubmitGroup(s, &fakeHolder{id: task.ID})
			if _, err := g.submitTaskResult(context.Background(), map[string]any{
				"summary": "验收完成", "verdict": verdict,
			}); err != nil {
				t.Fatalf("合法 verdict %q 被拒绝: %v", verdict, err)
			}
		})
	}

	for _, verdict := range []string{"fail", "blocked", "disputed", "PASS"} {
		t.Run("拒绝_"+verdict, func(t *testing.T) {
			s := store.NewMemoryTaskStore(nil, 8, 1, 60)
			task := publishPlainTask(t, s, &model.Task{Description: "acceptance node", EventType: "acceptance.verify", GraphID: "g-1"})
			g, notifier, state := newSubmitGroup(s, &fakeHolder{id: task.ID})
			_, err := g.submitTaskResult(context.Background(), map[string]any{
				"summary": "验收完成", "verdict": verdict,
			})
			if err == nil || !strings.Contains(err.Error(), "pass / fixable / failed") {
				t.Fatalf("非法 verdict %q 应在进入 finalizing 前拒绝，实际 %v", verdict, err)
			}
			if notifier.marked {
				t.Fatal("非法 verdict 不得进入 finalizing")
			}
			if _, ok := state.Take(task.ID); ok {
				t.Fatal("非法 verdict 不得写入 SubmitState")
			}
		})
	}

	s := store.NewMemoryTaskStore(nil, 8, 1, 60)
	task := publishPlainTask(t, s, &model.Task{Description: "acceptance node", EventType: "acceptance.verify", GraphID: "g-1"})
	g, notifier, state := newSubmitGroup(s, &fakeHolder{id: task.ID})
	_, err := g.submitTaskResult(context.Background(), map[string]any{
		"summary": "验收完成", "verdict": "pass", "event": "pass",
	})
	if err == nil || !strings.Contains(err.Error(), "verdict 与 event 互斥") {
		t.Fatalf("acceptance 双重结论字段应拒绝，实际 %v", err)
	}
	if notifier.marked {
		t.Fatal("双重结论字段不得进入 finalizing")
	}
	if _, ok := state.Take(task.ID); ok {
		t.Fatal("双重结论字段不得写入 SubmitState")
	}

	for _, field := range []string{"verdict", "event"} {
		t.Run("blocked_拒绝_"+field, func(t *testing.T) {
			s := store.NewMemoryTaskStore(nil, 8, 1, 60)
			task := publishPlainTask(t, s, &model.Task{Description: "graph node", EventType: "acceptance.verify", GraphID: "g-1"})
			g, notifier, state := newSubmitGroup(s, &fakeHolder{id: task.ID})
			args := map[string]any{
				"summary": "无法验收", "status": "blocked", "blocked_reason": "证据不足", field: "pass",
			}
			_, err := g.submitTaskResult(context.Background(), args)
			if err == nil || !strings.Contains(err.Error(), "status=blocked 时不得填写 verdict/event") {
				t.Fatalf("blocked 携带 %s 应拒绝，实际 %v", field, err)
			}
			if notifier.marked {
				t.Fatal("非法 blocked 协议不得进入 finalizing")
			}
			if _, ok := state.Take(task.ID); ok {
				t.Fatal("非法 blocked 协议不得写入 SubmitState")
			}
		})
	}
}

func TestSubmitTaskResultRejectsEmptySummary(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 8, 1, 60)
	task := publishPlainTask(t, s, &model.Task{Description: "d", EventType: "code"})
	g, notifier, _ := newSubmitGroup(s, &fakeHolder{id: task.ID})
	if _, err := g.submitTaskResult(context.Background(), map[string]any{"summary": "  "}); err == nil {
		t.Fatal("空白 summary 应报错")
	}
	if _, err := g.submitTaskResult(context.Background(), map[string]any{}); err == nil {
		t.Fatal("缺 summary 应报错")
	}
	if notifier.marked {
		t.Error("summary 为空时不应 MarkTaskFinalized")
	}
}

// ExpectedArtifacts 缺失：返回含失败原因的校验错误，且不标记 finalized——
// LLM 应在本轮循环内补写文件后重试。
func TestSubmitTaskResultRejectsMissingExpectedArtifacts(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 8, 1, 60)
	task := publishPlainTask(t, s, &model.Task{
		Description: "d", EventType: "code", ExpectedArtifacts: []string{"out.md"},
	})
	g, notifier, state := newSubmitGroup(s, &fakeHolder{id: task.ID})
	_, err := g.submitTaskResult(context.Background(), map[string]any{"summary": "假装完成"})
	if err == nil {
		t.Fatal("缺失 expected_artifacts 应拒绝")
	}
	if !strings.Contains(err.Error(), "缺失的预期文件") || !strings.Contains(err.Error(), "out.md") {
		t.Errorf("错误应包含 BuildArtifactFailureReason 文本，实际：%v", err)
	}
	if notifier.marked {
		t.Error("校验失败时不应 MarkTaskFinalized")
	}
	if _, ok := state.Take(task.ID); ok {
		t.Error("校验失败时不应暂存提交")
	}
}

func TestSubmitTaskResultDiskRecoverySynchronouslyRecordsArtifact(t *testing.T) {
	root := t.TempDir()
	content := []byte("前次尝试已写入")
	if err := os.WriteFile(filepath.Join(root, "out.md"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	s := store.NewMemoryTaskStore(nil, 8, 1, 60)
	task := publishPlainTask(t, s, &model.Task{
		Description: "disk recovery", EventType: "code", ExpectedArtifacts: []string{"out.md"},
	})
	g, notifier, _ := newSubmitGroup(s, &fakeHolder{id: task.ID})
	g.ArtifactResolver = func(string, string) string { return filepath.Join(root, "out.md") }

	if _, err := g.submitTaskResult(context.Background(), map[string]any{"summary": "恢复产物后提交"}); err != nil {
		t.Fatalf("submitTaskResult: %v", err)
	}
	if !notifier.marked {
		t.Fatal("恢复产物成功登记后应进入 finalizing")
	}
	got, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := model.ArtifactMeta{SHA256: computeSHA256(content), Bytes: int64(len(content))}
	if len(got.Artifacts) != 1 || got.Artifacts[0] != "out.md" || got.ArtifactMeta["out.md"] != want {
		t.Fatalf("磁盘恢复不得只放行校验，还必须在 finalizing 前补齐 artifact Evidence: artifacts=%v meta=%v", got.Artifacts, got.ArtifactMeta)
	}
}

func TestArtifactLedgerFailurePreventsSubmitFinalization(t *testing.T) {
	root := t.TempDir()
	artifactLog, err := store.OpenArtifactLog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := store.NewMemoryTaskStore(nil, 8, 1, 60)
	s.SetArtifactLog(artifactLog)
	task := publishPlainTask(t, s, &model.Task{
		Description: "closed artifact ledger", EventType: "code", ExpectedArtifacts: []string{"out.md"},
	})
	if err := artifactLog.Close(); err != nil {
		t.Fatal(err)
	}

	writeGroup := LocalWriteGroup{
		LocalReadGroup: LocalReadGroup{Workdir: &DefaultWorkdir{ProjectRoot: root}},
		Roster:         &recordingRoster{}, AgentID: "worker-1", ArtifactStore: s,
	}
	ctx := agent.WithAgentContext(context.Background(), "worker-1", task.ID, 1)
	if _, err := writeGroup.writeFile(ctx, map[string]any{"path": "out.md", "content": "文件写成但 ledger 失败"}); err == nil {
		t.Fatal("artifact ledger 失败时 write_file 必须报错")
	}

	g, notifier, state := newSubmitGroup(s, &fakeHolder{id: task.ID})
	g.ArtifactResolver = func(string, string) string { return filepath.Join(root, "out.md") }
	_, err = g.submitTaskResult(context.Background(), map[string]any{"summary": "不得绕过 ledger 收尾"})
	if err == nil || !strings.Contains(err.Error(), "durable artifact ledger") {
		t.Fatalf("同一响应中 write 失败后继续 submit 也必须被 ledger 挡住: %v", err)
	}
	if notifier.marked {
		t.Fatal("artifact ledger 未耐久化时不得进入 finalizing")
	}
	if _, ok := state.Take(task.ID); ok {
		t.Fatal("artifact ledger 失败时不得暂存结构化终态")
	}
}

// 非图任务带 request_replan=true（C6b）：提交生效的同时附带发布通用 replan
// 唤醒任务（与 request_replan 工具同机制，幂等键 <taskID>/replan），确认文本
// 附带登记提示。
func TestSubmitTaskResultNonGraphRequestReplanPublishesWake(t *testing.T) {
	s := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	task := publishPlainTask(t, s, &model.Task{Description: "d", EventType: "code"})
	g, notifier, state := newSubmitGroup(s, &fakeHolder{id: task.ID})
	reply, err := g.submitTaskResult(context.Background(), map[string]any{
		"summary": "完成但建议重估", "request_replan": true,
	})
	if err != nil {
		t.Fatalf("非图任务带 request_replan 不应报错: %v", err)
	}
	if !notifier.marked {
		t.Error("提交仍应生效")
	}
	sub, _ := state.Take(task.ID)
	if sub == nil || !sub.RequestReplan {
		t.Error("RequestReplan 标志应保留在结构化提交里")
	}
	if !strings.Contains(reply, "Replan 请求已登记") {
		t.Errorf("确认文本应附带 replan 登记提示，实际：%s", reply)
	}
	wakes := findReplanWake(t, s, replanRequestMarker(task.ID))
	if len(wakes) != 1 {
		t.Fatalf("应附带发布 1 个 replan 唤醒任务，实际 %d", len(wakes))
	}
	wake := wakes[0]
	if wake.EventSource != "replan-request" || wake.ParentTaskID != task.ID || wake.MaxConcurrency != 1 {
		t.Errorf("唤醒任务路由元数据错误: %+v", wake)
	}
	if !strings.Contains(wake.Description, "submit_request_replan") {
		t.Errorf("唤醒任务描述应含 reason_code=submit_request_replan，实际：%s", wake.Description)
	}
}

// 非图任务带 blocked_reason（C6b）：随提交发布高优 replan 唤醒任务
// （reason_code=submit_blocked，urgency=high，详情含 blocked_reason）。
func TestSubmitTaskResultBlockedPublishesHighUrgencyWake(t *testing.T) {
	s := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	task := publishPlainTask(t, s, &model.Task{Description: "implementation", EventType: "code"})
	g, notifier, _ := newSubmitGroup(s, &fakeHolder{id: task.ID})
	reply, err := g.submitTaskResult(context.Background(), map[string]any{
		"summary": "数据库迁移缺少权限", "blocked_reason": "无生产库写权限",
	})
	if err != nil {
		t.Fatalf("submitTaskResult: %v", err)
	}
	if !notifier.marked {
		t.Error("提交应生效")
	}
	if !strings.Contains(reply, "Replan 请求已登记") {
		t.Errorf("确认文本应附带 replan 登记提示，实际：%s", reply)
	}
	wakes := findReplanWake(t, s, replanRequestMarker(task.ID))
	if len(wakes) != 1 {
		t.Fatalf("应附带发布 1 个 replan 唤醒任务，实际 %d", len(wakes))
	}
	desc := wakes[0].Description
	for _, want := range []string{"submit_blocked", "high", "无生产库写权限", "数据库迁移缺少权限"} {
		if !strings.Contains(desc, want) {
			t.Errorf("唤醒任务描述缺少 %q，实际：%s", want, desc)
		}
	}
}

// 图任务带 blocked_reason/request_replan（C6b）：不发布 replan 唤醒任务——
// 图路由由 graph-terminal-feed 终态回填驱动，提交本身照常生效。
func TestSubmitTaskResultGraphTaskSkipsReplanWake(t *testing.T) {
	s := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	task := publishPlainTask(t, s, &model.Task{
		Description: "graph node", EventType: "code",
		GraphID: "g-1", NodeID: "implement", ActivationID: "implement@1",
	})
	g, notifier, _ := newSubmitGroup(s, &fakeHolder{id: task.ID})
	reply, err := g.submitTaskResult(context.Background(), map[string]any{
		"summary": "依赖缺失", "blocked_reason": "上游产物未就绪", "request_replan": true,
	})
	if err != nil {
		t.Fatalf("图任务提交不应报错: %v", err)
	}
	if !notifier.marked {
		t.Error("图任务提交仍应生效")
	}
	if strings.Contains(reply, "Replan 请求已登记") {
		t.Errorf("图任务不应附带 replan 登记提示，实际：%s", reply)
	}
	if wakes := findReplanWake(t, s, "[replan-request:"); len(wakes) != 0 {
		t.Errorf("图任务不应发布通用 replan 唤醒任务，实际 %d 个", len(wakes))
	}
	if wakes := findGraphChangeWake(t, s, "[graph-change-request:"); len(wakes) != 0 {
		t.Errorf("图任务提交不应发布 graph change 唤醒任务，实际 %d 个", len(wakes))
	}
}

// status=blocked（V6 §5）：结构化 blocked 提交被接受——Status 随提交暂存、
// finalized 标记、确认文本指向 blocked 终态收尾；工具层不再附带发布 replan
// 唤醒任务（终态落盘后的唤醒由 agent 收尾路径负责），并 emit task_finalizing。
func TestSubmitTaskResultStatusBlockedAccepted(t *testing.T) {
	d := installShellTraceCapture(t)
	s := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	task := publishPlainTask(t, s, &model.Task{Description: "数据库迁移", EventType: "code"})
	g, notifier, state := newSubmitGroup(s, &fakeHolder{id: task.ID})
	reply, err := g.submitTaskResult(context.Background(), map[string]any{
		"summary": "数据库迁移缺少权限", "blocked_reason": "无生产库写权限", "status": "blocked",
	})
	if err != nil {
		t.Fatalf("status=blocked 提交不应报错: %v", err)
	}
	if !notifier.marked {
		t.Error("提交应生效（MarkTaskFinalized）")
	}
	sub, ok := state.Take(task.ID)
	if !ok || sub == nil {
		t.Fatal("SubmitState 应暂存本次提交")
	}
	if sub.Status != "blocked" || sub.BlockedReason != "无生产库写权限" {
		t.Errorf("Status/BlockedReason = %q/%q，期望 blocked/无生产库写权限", sub.Status, sub.BlockedReason)
	}
	if !strings.Contains(reply, "blocked 终态") {
		t.Errorf("确认文本应指向 blocked 终态收尾，实际：%s", reply)
	}
	// 工具层不附带唤醒任务：终态落盘后的 replan 唤醒由 agent 收尾路径负责。
	if wakes := findReplanWake(t, s, replanRequestMarker(task.ID)); len(wakes) != 0 {
		t.Errorf("status=blocked 时工具层不应附带发布 replan 唤醒任务，实际 %d 个", len(wakes))
	}
	// task_finalizing：自述终态 blocked。
	var found *trace.Event
	for i, ev := range d.events {
		if ev.Kind == trace.KindTaskFinalizing && ev.TaskID == task.ID {
			found = &d.events[i]
			break
		}
	}
	if found == nil {
		t.Fatal("应 emit task_finalizing 事件")
	}
	if found.Transition == nil || found.Transition.NewStatus != "blocked" {
		t.Errorf("task_finalizing 应携带自述终态 blocked，实际 %+v", found.Transition)
	}
	if found.AgentID != "worker-1" {
		t.Errorf("task_finalizing.AgentID = %q，期望 worker-1", found.AgentID)
	}
}

// status=blocked 缺 blocked_reason：拒绝且不标记 finalized。
func TestSubmitTaskResultStatusBlockedRequiresReason(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 8, 1, 60)
	task := publishPlainTask(t, s, &model.Task{Description: "d", EventType: "code"})
	g, notifier, state := newSubmitGroup(s, &fakeHolder{id: task.ID})
	_, err := g.submitTaskResult(context.Background(), map[string]any{
		"summary": "无法完成", "status": "blocked",
	})
	if err == nil || !strings.Contains(err.Error(), "blocked_reason") {
		t.Fatalf("status=blocked 缺 blocked_reason 应报错，实际 err=%v", err)
	}
	if notifier.marked {
		t.Error("校验失败时不应 MarkTaskFinalized")
	}
	if _, ok := state.Take(task.ID); ok {
		t.Error("校验失败时不应暂存提交")
	}
}

// status 只接受 completed/blocked：failed、cancelled（系统路径专属）与
// 未知值一律拒绝。
func TestSubmitTaskResultStatusRejectsInvalidValues(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 8, 1, 60)
	for _, bad := range []string{"failed", "cancelled", "done"} {
		task := publishPlainTask(t, s, &model.Task{Description: "d-" + bad, EventType: "code"})
		g, notifier, _ := newSubmitGroup(s, &fakeHolder{id: task.ID})
		_, err := g.submitTaskResult(context.Background(), map[string]any{
			"summary": "s", "status": bad,
		})
		if err == nil || !strings.Contains(err.Error(), "completed / blocked") {
			t.Errorf("status=%q 应被拒绝并提示合法值，实际 err=%v", bad, err)
		}
		if notifier.marked {
			t.Errorf("status=%q 被拒绝时不应 MarkTaskFinalized", bad)
		}
	}
}

// status 缺省 = completed：行为与现网一致（暂存 Status=completed、正常发布
// request_replan 唤醒任务），task_finalizing 携带 completed。
func TestSubmitTaskResultDefaultStatusCompleted(t *testing.T) {
	d := installShellTraceCapture(t)
	s := store.NewMemoryTaskStore(make(chan model.Event, 16), 32, 1, 60)
	task := publishPlainTask(t, s, &model.Task{Description: "d", EventType: "code"})
	g, _, state := newSubmitGroup(s, &fakeHolder{id: task.ID})
	if _, err := g.submitTaskResult(context.Background(), map[string]any{
		"summary": "完成但建议重估", "request_replan": true,
	}); err != nil {
		t.Fatalf("submitTaskResult: %v", err)
	}
	sub, _ := state.Take(task.ID)
	if sub == nil || sub.Status != "completed" {
		t.Errorf("缺省 status 应归一为 completed，实际 %+v", sub)
	}
	if wakes := findReplanWake(t, s, replanRequestMarker(task.ID)); len(wakes) != 1 {
		t.Errorf("completed+request_replan 应照旧附带唤醒任务，实际 %d 个", len(wakes))
	}
	var found *trace.Event
	for i, ev := range d.events {
		if ev.Kind == trace.KindTaskFinalizing && ev.TaskID == task.ID {
			found = &d.events[i]
			break
		}
	}
	if found == nil || found.Transition == nil || found.Transition.NewStatus != "completed" {
		t.Errorf("task_finalizing 应携带自述终态 completed，实际 %+v", found)
	}
}

// 唯一终态提交者：已 finalized 后再次调用 submit_task_result 返回中文错误，
// 不重复提交、不改变已暂存的首次提交。使用真实 FinalizationHolder（实现
// IsFinalized）；旧式 fake notifier 不实现该接口时守卫退化为不检查。
func TestSubmitTaskResultRejectsDuplicateAfterFinalized(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 8, 1, 60)
	task := publishPlainTask(t, s, &model.Task{Description: "d", EventType: "code"})
	finHolder := agent.NewFinalizationHolder()
	finHolder.Set(task.ID)
	state := agent.NewSubmitState()
	g := PlanControlGroup{
		Store: s, Holder: &fakeHolder{id: task.ID}, AgentID: "worker-1",
		FinalizationNotifier: finHolder, SubmitState: state,
	}
	if _, err := g.submitTaskResult(context.Background(), map[string]any{"summary": "首次提交"}); err != nil {
		t.Fatalf("首次提交应成功: %v", err)
	}
	_, err := g.submitTaskResult(context.Background(), map[string]any{"summary": "重复提交"})
	if err == nil || !strings.Contains(err.Error(), "只能成功提交一次") {
		t.Fatalf("重复提交应返回中文错误，实际 err=%v", err)
	}
	sub, ok := state.Take(task.ID)
	if !ok || sub.Summary != "首次提交" {
		t.Errorf("首次提交不应被覆盖，实际 %+v (ok=%t)", sub, ok)
	}
}

// cited_evidence：逗号分隔的 EvidenceRef 清单原样暂存（经
// StructuredSubmission 写入 Results["cited_evidence"] 供图侧谱系核验）；
// 含空白引用项时在提交边界拒绝——任务保持未 finalized，agent 本轮内可
// 修正后重新提交。
func TestSubmitTaskResultCitedEvidence(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 8, 1, 60)
	task := publishPlainTask(t, s, &model.Task{Description: "acceptance", EventType: "acceptance.verify", GraphID: "g-1"})
	g, notifier, state := newSubmitGroup(s, &fakeHolder{id: task.ID})

	// 含空白引用项：拒绝、不标记 finalized、不暂存。
	_, err := g.submitTaskResult(context.Background(), map[string]any{
		"summary": "验收完成", "verdict": "pass", "cited_evidence": "ev:task-1:1,,ev:task-1:2",
	})
	if err == nil || !strings.Contains(err.Error(), "cited_evidence") {
		t.Fatalf("含空白引用项应被拒绝并点名参数，实际 err=%v", err)
	}
	if notifier.marked {
		t.Error("拒绝时不应 MarkTaskFinalized")
	}
	if _, ok := state.Take(task.ID); ok {
		t.Error("拒绝时不应暂存提交")
	}

	// 合法引用清单：接受并原样暂存（首尾空白已规范化）。
	valid := "  ev:task-1:1, ev:task-1:2  "
	if _, err := g.submitTaskResult(context.Background(), map[string]any{
		"summary": "验收完成", "verdict": "pass", "cited_evidence": valid,
	}); err != nil {
		t.Fatalf("合法 cited_evidence 应被接受: %v", err)
	}
	sub, ok := state.Take(task.ID)
	if !ok {
		t.Fatal("SubmitState 应暂存本次提交")
	}
	if sub.CitedEvidence != strings.TrimSpace(valid) {
		t.Errorf("CitedEvidence 应原样保留引用清单，实际 %q", sub.CitedEvidence)
	}
}
