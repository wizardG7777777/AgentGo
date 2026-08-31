package bootstrap

// 数据流（Result→Input 绑定）的 bootstrap 侧测试：
//   - graphTaskDescription 只保留 objective/control；graphTaskContextInputs 把
//     Result/Evidence 变成独立 typed L2 端口；
//   - assembleTaskEvidence 从 ToolCallRecord + Artifacts 组装稳定引用
//     （基于 CallID/内容身份而非查询序数）的有界证据条目，超上限追加截断标记。

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"agentgo/internal/checkstore"
	"agentgo/internal/graph"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

func TestGraphTaskContextInputsSeparateDataFromDescription(t *testing.T) {
	spec := graph.TaskSpec{
		Title: "验收修改", Description: "判据：编译通过",
		Inputs: []graph.InputBinding{
			{
				SourceNodeID: "implement", SourceActivationID: "implement@1",
				TargetInput:  "implementation",
				Summary:      "{\"note\":\"第一版\"}",
				Result:       json.RawMessage(`{"note":"第一版","coverage":87,"ready":true}`),
				EvidenceRefs: []string{"ev:task-1:1", "ev:task-1:2"},
			},
			{
				SourceNodeID: "probe", SourceActivationID: "probe@2",
				Summary: "{\"blob\":\"…\"}", Truncated: true,
			},
		},
	}
	desc := graphTaskDescription(spec)
	if desc != "验收修改\n\n判据：编译通过" {
		t.Errorf("Description 只应包含 objective/control: %q", desc)
	}
	inputs := graphTaskContextInputs(spec)
	if len(inputs) != 3 || inputs[0].Kind != model.TaskContextUpstreamResult ||
		inputs[1].Kind != model.TaskContextUpstreamEvidence || inputs[2].Kind != model.TaskContextUpstreamResult {
		t.Fatalf("Result typed inputs 形状错误: %+v", inputs)
	}
	joined := inputs[0].Content + inputs[1].Content + inputs[2].Content
	for _, want := range []string{
		`"source_node_id":"implement"`, `"source_activation_id":"implement@1"`,
		`"target_input":"implementation"`, "第一版", `"coverage":87`, `"ready":true`,
		`"source_node_id":"probe"`, `"truncated":true`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("typed Result 应包含 %q: %q", want, joined)
		}
	}

	// 无输入绑定：保持原有拼接待命（判据+标题），不追加数据流段落。
	plain := graphTaskDescription(graph.TaskSpec{Title: "理解需求", Description: "分解目标"})
	if plain != "理解需求\n\n分解目标" {
		t.Errorf("无输入时描述应为原标题+描述: %q", plain)
	}
}

func TestGraphTaskContextInputsCarriesEvidenceWithoutSideChannelTool(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
	source := &model.Task{Description: "实现", Artifacts: []string{"out/report.md"}}
	if err := s.PublishTask(source); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	exit := 0
	if err := s.AppendToolCall(source.ID, store.ToolCallRecord{
		Timestamp: time.Now(), CallID: "call-check", AgentID: "worker-1",
		ToolName: "run_shell", Args: map[string]any{"command": "go test ./..."},
		Success: true, ExitCode: &exit,
	}); err != nil {
		t.Fatalf("AppendToolCall: %v", err)
	}
	evidence := assembleTaskEvidence(s, source)
	if len(evidence) != 2 {
		t.Fatalf("证据数量=%d，期望调用+artifact 两条: %+v", len(evidence), evidence)
	}

	spec := graph.TaskSpec{Title: "验收", Inputs: []graph.InputBinding{{
		SourceNodeID: "implement", SourceActivationID: "implement@1",
		Evidence:     evidence,
		EvidenceRefs: []string{evidence[0].Ref, evidence[1].Ref, "ev:missing:call:deadbeef"},
	}}}
	inputs := graphTaskContextInputs(spec)
	if len(inputs) != 2 || inputs[1].Kind != model.TaskContextUpstreamEvidence {
		t.Fatalf("Evidence typed input 缺失: %+v", inputs)
	}
	content := inputs[1].Content
	for _, want := range []string{
		evidence[0].Ref, `"kind":"shell"`, "go test ./...", `"exit_code":0`,
		`"call_id":"call-check"`, `"tool_name":"run_shell"`, `"success":true`,
		evidence[1].Ref, `"kind":"artifact"`, `"path":"out/report.md"`,
		`"unresolved_evidence_refs":["ev:missing:call:deadbeef"]`,
	} {
		if !strings.Contains(content, want) {
			t.Errorf("冻结 Evidence input 应包含 %q: %s", want, content)
		}
	}
	if strings.Contains(content, "read_graph") || strings.Contains(content, "get_task_result") {
		t.Errorf("数据流任务不得要求 verifier 使用旁路图查询工具: %s", content)
	}
}

func TestGraphTaskContextInputsCarriesFulfillmentCheckEvidence(t *testing.T) {
	task := &model.Task{ID: "task-check", Description: "实现"}
	record := checkstore.Record{
		CheckRef: "check:sha256:abc", CheckID: "verification", Kind: "test",
		Status: checkstore.StatusPass, ExitCode: 0, ExitCodeScope: "whole_command",
		WorkspaceRevisionRef: "workspace:sha256:candidate", OutputRef: "content:sha256:output",
	}
	evidence := assembleTaskEvidenceFromCallsAndChecks(task, nil, []checkstore.Record{record})
	if len(evidence) != 1 || evidence[0].Kind != "check" || evidence[0].CheckRef != record.CheckRef {
		t.Fatalf("typed Check Evidence 组装失败: %+v", evidence)
	}
	inputs := graphTaskContextInputs(graph.TaskSpec{Title: "验收", Inputs: []graph.InputBinding{{
		SourceNodeID: "work", SourceActivationID: "work@1",
		Evidence: evidence, EvidenceRefs: []string{evidence[0].Ref},
	}}})
	content := inputs[0].Content + inputs[1].Content
	for _, want := range []string{
		evidence[0].Ref, `"kind":"check"`, `"check_ref":"check:sha256:abc"`,
		`"check_id":"verification"`, `"check_status":"pass"`,
		`"workspace_revision_ref":"workspace:sha256:candidate"`, `"exit_code":0`,
	} {
		if !strings.Contains(content, want) {
			t.Errorf("冻结 Check Evidence 缺少 %q: %s", want, content)
		}
	}
}

func TestFulfillmentCheckEvidenceSurvivesRawEvidenceTruncation(t *testing.T) {
	task := &model.Task{ID: "task-many-calls", Description: "实现", Artifacts: []string{"out.txt"}}
	calls := make([]store.ToolCallRecord, 0, evidenceMaxEntries+5)
	for index := 0; index < evidenceMaxEntries+5; index++ {
		calls = append(calls, store.ToolCallRecord{
			CallID: fmt.Sprintf("call-%03d", index), ToolName: "read_file",
			Args: map[string]any{"path": fmt.Sprintf("file-%03d", index)}, Success: true,
		})
	}
	check := checkstore.Record{
		CheckRef: "check:sha256:required", CheckID: "verification", Kind: "test",
		Status: checkstore.StatusPass, ExitCode: 0, ExitCodeScope: "whole_command",
		WorkspaceRevisionRef: "workspace:sha256:candidate",
	}
	evidence := assembleTaskEvidenceFromCallsAndChecks(task, calls, []checkstore.Record{check})
	foundCheck, foundArtifact, foundTruncated := false, false, false
	for _, entry := range evidence {
		foundCheck = foundCheck || entry.Kind == "check" && entry.CheckRef == check.CheckRef
		foundArtifact = foundArtifact || entry.Kind == "artifact" && entry.Path == "out.txt"
		foundTruncated = foundTruncated || entry.Kind == "truncated"
	}
	if !foundCheck || !foundArtifact || !foundTruncated {
		t.Fatalf("必需 Artifact/Check Evidence 不得被 raw 调用上限截掉: %+v", evidence)
	}
}

func TestGraphTaskContextInputsUsesDurableEvidenceWithoutSourceTask(t *testing.T) {
	entry := graph.EvidenceEntry{
		Ref: "ev:gone-task:call:0123456789abcdef", Kind: "shell",
		Summary: "命令: go test ./...（exit=0）",
	}
	inputs := graphTaskContextInputs(graph.TaskSpec{Title: "恢复后验收", Inputs: []graph.InputBinding{{
		SourceNodeID: "checker", SourceActivationID: "checker@7",
		ResultRef: "result:g-1:checker@7", Truncated: true,
		Evidence: []graph.EvidenceEntry{entry}, EvidenceRefs: []string{entry.Ref},
	}}})
	content := inputs[0].Content + inputs[1].Content
	for _, want := range []string{
		`"result_ref":"result:g-1:checker@7"`, entry.Ref, `"kind":"shell"`, "go test ./...",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("源 Task 已淘汰时应直接消费 InputBinding 的 durable Evidence %q: %s", want, content)
		}
	}
	if strings.Contains(content, "unresolved_evidence_refs") {
		t.Errorf("已随 InputBinding 持久化的 Evidence 不应标 unresolved: %s", content)
	}
}

func TestGraphTaskDescriptionRendersAcceptanceEvidenceContractAndMissingFacts(t *testing.T) {
	spec := graph.TaskSpec{
		Title: "验收交易日志",
		RequiredEvidence: []graph.EvidenceRequirement{
			{Input: "implementation", Kind: "artifact"},
			{Input: "test_run", Kind: "shell"},
		},
		MissingEvidence: []graph.EvidenceRequirement{{Input: "test_run", Kind: "shell"}},
	}
	desc := graphTaskDescription(spec)
	for _, want := range []string{
		"验收证据契约", "输入端口 implementation", "kind=artifact",
		"当前证据缺口", "输入端口 test_run 缺少 kind=shell",
		"必须以 blocked 结算", "不得提交 pass",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("验收任务描述缺少冻结证据契约/缺口 %q: %s", want, desc)
		}
	}

	ready := spec
	ready.MissingEvidence = nil
	readyDesc := graphTaskDescription(ready)
	if strings.Contains(readyDesc, "必须以 blocked 结算") {
		t.Errorf("证据齐备时不得注入 blocked 强制处置: %s", readyDesc)
	}
}

func TestGraphBoardPublishFreezesResultAndResolvedEvidence(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
	source := &model.Task{Description: "实现"}
	if err := s.PublishTask(source); err != nil {
		t.Fatalf("PublishTask(source): %v", err)
	}
	exit := 0
	if err := s.AppendToolCall(source.ID, store.ToolCallRecord{
		Timestamp: time.Now(), CallID: "call-build", AgentID: "worker",
		ToolName: "run_shell", Args: map[string]any{"command": "go build ./..."},
		Success: true, ExitCode: &exit,
	}); err != nil {
		t.Fatalf("AppendToolCall: %v", err)
	}
	ref := assembleTaskEvidence(s, source)[0].Ref
	board := newGraphBoard(s)
	id, err := board.PublishGraphTask(graph.TaskSpec{
		GraphID: "g-freeze", NodeID: "accept", ActivationID: "accept@1",
		NodeKind: graph.KindAcceptance, Title: "验收", Route: graph.RouteAcceptance,
		Inputs: []graph.InputBinding{{
			SourceNodeID: "implement", SourceActivationID: "implement@1",
			Result:   json.RawMessage(`{"coverage":91,"checks":["build"]}`),
			Evidence: assembleTaskEvidence(s, source), EvidenceRefs: []string{ref},
		}},
	})
	if err != nil {
		t.Fatalf("PublishGraphTask: %v", err)
	}
	task, err := s.GetTask(id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	joined := ""
	for _, input := range task.ContextInputs {
		joined += input.Content
	}
	for _, want := range []string{`"coverage":91`, ref, `"kind":"shell"`, "go build ./...", `"exit_code":0`} {
		if !strings.Contains(joined, want) {
			t.Errorf("真实发布的冻结 typed input 缺少 %q: %s", want, joined)
		}
	}
	if strings.Contains(task.Description, `"coverage":91`) {
		t.Errorf("Graph Result 不得污染 Description: %s", task.Description)
	}
}

func TestAssembleTaskEvidenceRefStableAcrossQueryOrder(t *testing.T) {
	stamp := time.Now()
	makeStore := func(reverse bool) (*store.MemoryTaskStore, *model.Task) {
		s := store.NewMemoryTaskStore(nil, 100, 1, 300)
		task := &model.Task{ID: "stable-task", Description: "检查"}
		if err := s.PublishTask(task); err != nil {
			t.Fatalf("PublishTask: %v", err)
		}
		calls := []store.ToolCallRecord{
			{Timestamp: stamp, CallID: "call-b", AgentID: "worker", ToolName: "read_file", Args: map[string]any{"path": "b.go"}, Success: true},
			{Timestamp: stamp, CallID: "call-a", AgentID: "worker", ToolName: "run_shell", Args: map[string]any{"command": "go test ./..."}, Success: true},
		}
		if reverse {
			calls[0], calls[1] = calls[1], calls[0]
		}
		for _, call := range calls {
			if err := s.AppendToolCall(task.ID, call); err != nil {
				t.Fatalf("AppendToolCall: %v", err)
			}
		}
		return s, task
	}

	s1, task1 := makeStore(false)
	s2, task2 := makeStore(true)
	one := assembleTaskEvidence(s1, task1)
	two := assembleTaskEvidence(s2, task2)
	if len(one) != len(two) || len(one) != 2 {
		t.Fatalf("证据数量不一致: one=%+v two=%+v", one, two)
	}
	for i := range one {
		if !reflect.DeepEqual(one[i], two[i]) {
			t.Fatalf("相同 durable 调用事实不应因写入/查询顺序改变 Ref: one=%+v two=%+v", one, two)
		}
		if strings.HasSuffix(one[i].Ref, ":1") || strings.HasSuffix(one[i].Ref, ":2") {
			t.Errorf("EvidenceRef 不得使用展示序数作为身份: %q", one[i].Ref)
		}
	}
}

func TestGraphTaskLargeInputStaysOutOfDescriptionForL2Budgeting(t *testing.T) {
	large := strings.Repeat("界", 128<<10)
	spec := graph.TaskSpec{Title: "消费", Inputs: []graph.InputBinding{{
		SourceNodeID: "large", SourceActivationID: "large@1",
		Result: json.RawMessage(`{"content":"` + large + `"}`),
	}}}
	desc := graphTaskDescription(spec)
	inputs := graphTaskContextInputs(spec)
	if desc != "消费" || strings.Contains(desc, large[:128]) {
		t.Fatalf("大输入不得拼入 Description/user_task: len=%d", len(desc))
	}
	if len(inputs) != 1 || inputs[0].Kind != model.TaskContextUpstreamResult || !strings.Contains(inputs[0].Content, large[:128]) {
		t.Fatalf("大输入应完整交给 L2 typed port 决定 reference/drop: inputs=%d", len(inputs))
	}
}

func TestAssembleTaskEvidence(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
	task := &model.Task{Description: "实现", Artifacts: []string{"out/report.md"}}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	exit1 := 1
	exit0 := 0
	base := time.Now()
	calls := []store.ToolCallRecord{
		{Timestamp: base, CallID: "call-test", AgentID: "a1", ToolName: "run_shell",
			Args: map[string]any{"command": "go test ./..."}, Success: true, ExitCode: &exit1},
		{Timestamp: base.Add(time.Millisecond), CallID: "call-write", AgentID: "a1", ToolName: "write_file",
			Args: map[string]any{"path": "out/report.md"}, Success: true},
		{Timestamp: base.Add(2 * time.Millisecond), CallID: "call-build", AgentID: "a1", ToolName: "run_shell",
			Args: map[string]any{"command": "go build ./..."}, Success: true, ExitCode: &exit0},
	}
	for _, c := range calls {
		if err := s.AppendToolCall(task.ID, c); err != nil {
			t.Fatalf("AppendToolCall: %v", err)
		}
	}

	ev := assembleTaskEvidence(s, task)
	if len(ev) != 4 {
		t.Fatalf("应为 3 条调用证据 + 1 条 artifact，实际 %d: %+v", len(ev), ev)
	}
	if !strings.HasPrefix(ev[0].Ref, "ev:"+task.ID+":call:") || ev[0].Kind != "shell" ||
		!strings.Contains(ev[0].Summary, "go test ./...") || !strings.Contains(ev[0].Summary, "exit=1") {
		t.Errorf("shell 证据应含稳定引用/种类/命令/退出码: %+v", ev[0])
	}
	if ev[0].CallID != "call-test" || ev[0].ToolName != "run_shell" || ev[0].Success == nil || !*ev[0].Success ||
		ev[0].Command != "go test ./..." || ev[0].CommandTruncated || ev[0].ExitCode == nil || *ev[0].ExitCode != 1 {
		t.Errorf("shell 证据结构化字段不完整: %+v", ev[0])
	}
	if !strings.HasPrefix(ev[1].Ref, "ev:"+task.ID+":call:") || ev[1].Kind != "file_write" || !strings.Contains(ev[1].Summary, "out/report.md") {
		t.Errorf("file_write 证据应含路径: %+v", ev[1])
	}
	if ev[1].Path != "out/report.md" || ev[1].PathTruncated {
		t.Errorf("file_write 应保留独立完整 path: %+v", ev[1])
	}
	if ev[2].Kind != "shell" || !strings.Contains(ev[2].Summary, "exit=0") {
		t.Errorf("第二条 shell 证据应含退出码: %+v", ev[2])
	}
	if ev[3].Kind != "artifact" || !strings.HasPrefix(ev[3].Ref, "ev:"+task.ID+":artifact:") || !strings.Contains(ev[3].Summary, "out/report.md") {
		t.Errorf("artifact 证据应含产物路径: %+v", ev[3])
	}
	if ev[3].Path != "out/report.md" || ev[3].PathTruncated {
		t.Errorf("artifact path 不得仅存在于 200-rune Summary: %+v", ev[3])
	}
}

func TestAssembleTaskEvidenceStructuredFieldsAreBounded(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
	longCommand := strings.Repeat("测", graph.EvidenceCommandMaxRunes+100)
	longPath := strings.Repeat("路", graph.EvidencePathMaxRunes+100) + ".log"
	task := &model.Task{Description: "边界", Artifacts: []string{longPath}}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	exit := 17
	if err := s.AppendToolCall(task.ID, store.ToolCallRecord{
		Timestamp: time.Now(), CallID: "call-long", ToolName: "run_shell",
		Args: map[string]any{"command": longCommand}, Success: false, ExitCode: &exit,
	}); err != nil {
		t.Fatalf("AppendToolCall: %v", err)
	}
	ev := assembleTaskEvidence(s, task)
	if len(ev) != 2 {
		t.Fatalf("证据数量=%d，want 2: %+v", len(ev), ev)
	}
	if got := len([]rune(ev[0].Command)); got != graph.EvidenceCommandMaxRunes || !ev[0].CommandTruncated {
		t.Errorf("command 边界/标志错误: runes=%d evidence=%+v", got, ev[0])
	}
	if ev[0].Success == nil || *ev[0].Success || ev[0].ExitCode == nil || *ev[0].ExitCode != 17 {
		t.Errorf("失败与非零退出码必须结构化保留: %+v", ev[0])
	}
	if got := len([]rune(ev[1].Path)); got != graph.EvidencePathMaxRunes || !ev[1].PathTruncated {
		t.Errorf("artifact path 边界/标志错误: runes=%d evidence=%+v", got, ev[1])
	}
	inputs := graphTaskContextInputs(graph.TaskSpec{Title: "核验边界", Inputs: []graph.InputBinding{{
		SourceNodeID: "checker", SourceActivationID: "checker@1",
		Evidence: ev, EvidenceRefs: []string{ev[0].Ref, ev[1].Ref},
	}}})
	content := inputs[1].Content
	for _, want := range []string{`"success":false`, `"exit_code":17`, `"command_truncated":true`, `"path_truncated":true`} {
		if !strings.Contains(content, want) {
			t.Errorf("冻结 Evidence input 必须显式呈现结构化证据边界 %q", want)
		}
	}
}

func TestAssembleTaskEvidenceTruncates(t *testing.T) {
	s := store.NewMemoryTaskStore(nil, 100, 1, 300)
	task := &model.Task{Description: "批量"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	for i := 0; i < evidenceMaxEntries+10; i++ {
		if err := s.AppendToolCall(task.ID, store.ToolCallRecord{
			Timestamp: time.Now(), CallID: fmt.Sprintf("call-%03d", i), ToolName: "read_file",
		}); err != nil {
			t.Fatalf("AppendToolCall: %v", err)
		}
	}
	ev := assembleTaskEvidence(s, task)
	if len(ev) != evidenceMaxEntries+1 {
		t.Fatalf("应截断为 %d 条 + 1 条标记，实际 %d", evidenceMaxEntries, len(ev))
	}
	last := ev[len(ev)-1]
	if last.Kind != "truncated" || !strings.Contains(last.Summary, "从略") {
		t.Errorf("末条应为截断标记: %+v", last)
	}
}
