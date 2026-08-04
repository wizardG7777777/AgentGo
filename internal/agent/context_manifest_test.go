package agent

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"agentgo/internal/llm"
	"agentgo/internal/model"
	"agentgo/internal/trace"
)

// --- ManifestBuilder 单元行为 ---

// TestManifestBuilder_RegisterSeal 验证 Builder 基本语义：逐项登记、digest
// 稳定（sha256 前 12 位）、tokens 按 rune/3 估算、Seal 汇总且不可变。
func TestManifestBuilder_RegisterSeal(t *testing.T) {
	b := NewManifestBuilder("task-1", 0)
	b.Register(ManifestSectionSystemPrompt, SourceAgentPrompt, ScopeProcess,
		AuthorityAuthoritative, FreshnessLive, DispositionIncluded, "系统提示")
	b.Register(ManifestSectionTaskDesc, SourceUserInput, ScopeTask,
		AuthorityAuthoritative, FreshnessLive, DispositionIncluded, "写一份报告 abc")

	m := b.Seal()
	if m.TaskID != "task-1" || m.Loop != 0 {
		t.Fatalf("Manifest 头 = %+v, want task-1/0", m)
	}
	if len(m.Items) != 2 {
		t.Fatalf("Items 数 = %d, want 2", len(m.Items))
	}
	// 登记顺序保持：system_prompt 在前（装配优先级注释见 context_manifest.go）。
	if m.Items[0].ID != ManifestSectionSystemPrompt || m.Items[1].ID != ManifestSectionTaskDesc {
		t.Fatalf("Items 顺序 = [%s %s], want [system_prompt task_desc]", m.Items[0].ID, m.Items[1].ID)
	}

	// digest：与同内容独立计算一致（稳定性）；不同内容不同 digest。
	wantDigest := manifestDigest("系统提示")
	if m.Items[0].Digest != wantDigest {
		t.Errorf("system_prompt digest = %q, want %q", m.Items[0].Digest, wantDigest)
	}
	b2 := NewManifestBuilder("task-1", 0)
	b2.Register(ManifestSectionSystemPrompt, SourceAgentPrompt, ScopeProcess,
		AuthorityAuthoritative, FreshnessLive, DispositionIncluded, "系统提示")
	if got := b2.Seal().Items[0].Digest; got != wantDigest {
		t.Errorf("同内容 digest 不稳定: %q vs %q", got, wantDigest)
	}

	// tokens：rune/3。"系统提示" 4 runes → 1；"写一份报告 abc" 8 runes → 2。
	if m.Items[0].Tokens != 1 {
		t.Errorf("system_prompt tokens = %d, want 1（rune/3）", m.Items[0].Tokens)
	}
	if m.Items[1].Tokens != len([]rune("写一份报告 abc"))/3 {
		t.Errorf("task_desc tokens = %d, want %d", m.Items[1].Tokens, len([]rune("写一份报告 abc"))/3)
	}
	if m.TotalEstimatedTokens != m.Items[0].Tokens+m.Items[1].Tokens {
		t.Errorf("TotalEstimatedTokens = %d, want 各项之和 %d",
			m.TotalEstimatedTokens, m.Items[0].Tokens+m.Items[1].Tokens)
	}

	// Seal 不可变：修改返回值不影响 Builder；再次 Seal 结果不变；Seal 后
	// Register 静默忽略。
	m.Items[0].Disposition = "被外部篡改"
	m2 := b.Seal()
	if m2.Items[0].Disposition != DispositionIncluded {
		t.Errorf("Seal 返回值被外部修改后影响了 Builder: %q", m2.Items[0].Disposition)
	}
	b.Register(ManifestSectionMailbox, SourceMailbox, ScopeTask,
		AuthorityUntrusted, FreshnessLive, DispositionIncluded, "晚到的段")
	if got := len(b.Seal().Items); got != 2 {
		t.Errorf("Seal 后 Register 应被忽略, Items = %d, want 2", got)
	}
}

// TestManifestBuilder_MergeSameID 验证同 ID 段重复登记合并：digest 基于全部
// 内容连接、tokens 累加、Count 累加、元数据以最后一次为准。
func TestManifestBuilder_MergeSameID(t *testing.T) {
	b := NewManifestBuilder("task-1", 1)
	b.Register(ManifestSectionMailbox, SourceMailbox, ScopeTask,
		AuthorityUntrusted, FreshnessLive, DispositionIncluded, "邮件一")
	b.Register(ManifestSectionMailbox, SourceMailbox, ScopeTask,
		AuthorityUntrusted, FreshnessLive, DispositionIncluded, "邮件二")

	m := b.Seal()
	if len(m.Items) != 1 {
		t.Fatalf("同 ID 应合并为 1 项, 实际 %d", len(m.Items))
	}
	item := m.Items[0]
	if item.Count != 2 {
		t.Errorf("Count = %d, want 2", item.Count)
	}
	if item.Digest != manifestDigest("邮件一", "邮件二") {
		t.Errorf("合并 digest = %q, want 内容连接的 digest %q", item.Digest, manifestDigest("邮件一", "邮件二"))
	}
	wantTokens := estimateManifestTokens("邮件一") + estimateManifestTokens("邮件二")
	if item.Tokens != wantTokens {
		t.Errorf("合并 tokens = %d, want %d", item.Tokens, wantTokens)
	}
}

// TestManifestBuilder_DispositionCoverage 验证各种 Disposition 取值原样入账。
func TestManifestBuilder_DispositionCoverage(t *testing.T) {
	b := NewManifestBuilder("task-1", 0)
	dispositions := []string{
		DispositionIncluded, DispositionCompressed, DispositionSnipped,
		DispositionTruncated, DispositionDroppedPrefix + "超预算",
		DispositionCompressed + ":summary+keep_recent=3",
	}
	for _, d := range dispositions {
		b.Register(ManifestSectionHistory, SourceHistory, ScopeTask,
			AuthorityInformational, FreshnessLive, d, "内容")
	}
	m := b.Seal()
	// 同 ID 合并为一项，Disposition 以最后一次登记为准。
	if len(m.Items) != 1 {
		t.Fatalf("Items = %d, want 1（同 ID 合并）", len(m.Items))
	}
	if m.Items[0].Disposition != DispositionCompressed+":summary+keep_recent=3" {
		t.Errorf("Disposition = %q, want 最后一次登记的 %q",
			m.Items[0].Disposition, DispositionCompressed+":summary+keep_recent=3")
	}
}

// --- buildContextManifest 装配 ---

// manifestTestSideCtx 构造带侧信息的 ctx：taskStartedAt=now，team 段 1 小时前
// 写入（stale），file 段刚刚写入（live）。
func manifestTestSideCtx(taskID string, loop int) (context.Context, *manifestSideInfo) {
	ctx := WithAgentContext(context.Background(), "agent-1", taskID, loop)
	info := newManifestSideInfo(time.Now())
	info.recordMemorySectionUpdatedAt(markerTeamSnapshot, time.Now().Add(-time.Hour))
	info.recordMemorySectionUpdatedAt(markerFileAwareness, time.Now())
	return withManifestSideInfo(ctx, info), info
}

func manifestItemByID(m ContextManifest, id string) *ContextItem {
	for i := range m.Items {
		if m.Items[i].ID == id {
			return &m.Items[i]
		}
	}
	return nil
}

// TestBuildContextManifest_FirstRoundSections 验证带依赖 + mailbox + memory 的
// 任务首轮 Manifest：段齐全且 Source/Scope/Authority/Freshness 正确。
func TestBuildContextManifest_FirstRoundSections(t *testing.T) {
	ctx, _ := manifestTestSideCtx("task-1", 0)
	task := &model.Task{ID: "task-1", Description: "写一份报告"}
	depResults := map[string]string{"dep-1": "上游结果"}
	history := []HistoryEntry{
		{IncomingMail: "<dep-task-memory>\n[from dep-1]\n<task-memory source=\"task-memory\" version=\"2\">\n目标: 调查\n</task-memory>\n</dep-task-memory>"},
		{IncomingMail: "<team-snapshot>\n队伍状态\n</team-snapshot>\n\n<file-awareness>\n文件占用\n</file-awareness>"},
		{IncomingMail: "<agent-mail type=\"info\" priority=\"normal\">\n  <from>scheduler @ 10:00:00</from>\n  <body>请加快</body>\n</agent-mail>"},
	}
	toolDefs := []llm.ToolDef{
		{Name: "read_file", Description: "读取文件"},
		{Name: "write_file", Description: "写入文件"},
	}

	m := buildContextManifest(ctx, "系统提示", task, depResults, history, "", toolDefs)

	// 段齐全与登记顺序：契约层 → 任务 → 依赖 → 历史流注入段 → 工具协议。
	wantOrder := []string{
		ManifestSectionSystemPrompt, ManifestSectionTaskContext, ManifestSectionTaskDesc,
		ManifestSectionDepResults, ManifestSectionDepTaskMemory,
		ManifestSectionMemoryTeamSnapshot, ManifestSectionMemoryFileAwareness,
		ManifestSectionMailbox, ManifestSectionToolsSchema,
	}
	if len(m.Items) != len(wantOrder) {
		t.Fatalf("Items = %d, want %d: %+v", len(m.Items), len(wantOrder), m.Items)
	}
	for i, id := range wantOrder {
		if m.Items[i].ID != id {
			t.Fatalf("Items[%d].ID = %q, want %q（全量: %+v）", i, m.Items[i].ID, id, m.Items)
		}
	}

	type wantMeta struct{ source, scope, authority, freshness string }
	want := map[string]wantMeta{
		ManifestSectionSystemPrompt:        {SourceAgentPrompt, ScopeProcess, AuthorityAuthoritative, FreshnessLive},
		ManifestSectionTaskContext:         {SourceControlPlane, ScopeTask, AuthorityAuthoritative, FreshnessLive},
		ManifestSectionTaskDesc:            {SourceUserInput, ScopeTask, AuthorityAuthoritative, FreshnessLive},
		ManifestSectionDepResults:          {SourceDependency, ScopeTask, AuthorityInformational, FreshnessSnapshot},
		ManifestSectionDepTaskMemory:       {SourceTaskMemory, ScopeTask, AuthorityInformational, FreshnessSnapshot},
		ManifestSectionMemoryTeamSnapshot:  {SourceMemory, ScopeProcess, AuthorityInformational, FreshnessStale},
		ManifestSectionMemoryFileAwareness: {SourceMemory, ScopeProcess, AuthorityInformational, FreshnessLive},
		ManifestSectionMailbox:             {SourceMailbox, ScopeTask, AuthorityUntrusted, FreshnessLive},
		ManifestSectionToolsSchema:         {SourceTools, ScopeProcess, AuthorityAuthoritative, FreshnessLive},
	}
	for id, w := range want {
		item := manifestItemByID(m, id)
		if item == nil {
			t.Errorf("缺少段 %s", id)
			continue
		}
		if item.Source != w.source || item.Scope != w.scope || item.Authority != w.authority || item.Freshness != w.freshness {
			t.Errorf("段 %s 元数据 = {%s %s %s %s}, want {%s %s %s %s}",
				id, item.Source, item.Scope, item.Authority, item.Freshness,
				w.source, w.scope, w.authority, w.freshness)
		}
		if item.Disposition != DispositionIncluded {
			t.Errorf("段 %s Disposition = %q, want included", id, item.Disposition)
		}
		if item.Digest == "" || item.Tokens <= 0 {
			t.Errorf("段 %s digest/tokens 未填充: %+v", id, item)
		}
	}

	// 计数：dep_results=依赖数，tools_schema=工具数。
	if got := manifestItemByID(m, ManifestSectionDepResults).Count; got != 1 {
		t.Errorf("dep_results Count = %d, want 1", got)
	}
	if got := manifestItemByID(m, ManifestSectionToolsSchema).Count; got != 2 {
		t.Errorf("tools_schema Count = %d, want 2", got)
	}

	// memory 拆分：两段各自 digest 对应拆分后的内容（joinSections 的合并文本
	// 不能整段算到某一个段上）。
	if got := manifestItemByID(m, ManifestSectionMemoryTeamSnapshot).Digest; got != manifestDigest("<team-snapshot>\n队伍状态\n</team-snapshot>") {
		t.Errorf("memory_team_snapshot digest = %q, want 拆分后 team 段的 digest", got)
	}
}

// TestBuildContextManifest_SystemPromptOverride 验证任务级 SystemPrompt 覆盖时
// system_prompt 段 Source 切换为 control-plane（内容来自任务发布方）。
func TestBuildContextManifest_SystemPromptOverride(t *testing.T) {
	ctx := WithAgentContext(context.Background(), "agent-1", "task-2", 0)
	task := &model.Task{ID: "task-2", Description: "d", SystemPrompt: "覆盖提示"}

	m := buildContextManifest(ctx, "覆盖提示", task, nil, nil, "", nil)
	item := manifestItemByID(m, ManifestSectionSystemPrompt)
	if item == nil {
		t.Fatal("缺少 system_prompt 段")
	}
	if item.Source != SourceControlPlane {
		t.Errorf("覆盖时 Source = %q, want control-plane", item.Source)
	}
	if item.Digest != manifestDigest("覆盖提示") {
		t.Errorf("digest 应对应实际生效的覆盖提示")
	}
}

// TestBuildContextManifest_RetryRound 验证重试轮（恢复的 LastHistory 主体 +
// 未识别注入段兜底）段变化：history 主体条数正确，未识别注入落入
// injected_segment（V6 CM4 起 transfer-note 注入已删除）。
func TestBuildContextManifest_RetryRound(t *testing.T) {
	ctx, _ := manifestTestSideCtx("task-3", 0)
	task := &model.Task{ID: "task-3", Description: "d", RetryCount: 1}
	history := []HistoryEntry{
		{IncomingMail: "<scheduler-note>\n重试提醒\n</scheduler-note>"},
		{Output: "=== 历史摘要 ===\n步骤 1: [read_file] 读了 a.go\n", ToolCalled: false},
		{ToolCalled: true, AssistantContent: "继续",
			ToolCalls:   []llm.ToolCall{{ID: "c1", Name: "read_file", Arguments: map[string]any{"path": "a.go"}}},
			ToolResults: []ToolResult{{ToolCallID: "c1", Content: "文件内容"}}},
	}

	m := buildContextManifest(ctx, "sys", task, nil, history, "", nil)

	injected := manifestItemByID(m, ManifestSectionInjectedSegment)
	if injected == nil {
		t.Fatal("未识别注入应落入 injected_segment 段")
	}
	if injected.Source != SourceControlPlane || injected.Authority != AuthorityInformational {
		t.Errorf("injected_segment = {%s %s}, want {control-plane informational}", injected.Source, injected.Authority)
	}

	hist := manifestItemByID(m, ManifestSectionHistory)
	if hist == nil {
		t.Fatal("缺少 history 主体段")
	}
	if hist.Count != 2 {
		t.Errorf("history Count = %d, want 2（摘要条 + 工具轮）", hist.Count)
	}
	// 恢复的 LastHistory 首条是压缩摘要 → Disposition=compressed
	//（跨 attempt 无法区分 L2/L3，统一 compressed，见 historyDisposition 注释）。
	if hist.Disposition != DispositionCompressed {
		t.Errorf("history Disposition = %q, want compressed（摘要条目检测）", hist.Disposition)
	}
}

// TestBuildContextManifest_CompressionDispositions 验证压缩处置回填优先级：
// truncated(L3) > compressed:strategy(L2) > compressed(摘要检测) > snipped(L1 墓碑) > included。
func TestBuildContextManifest_CompressionDispositions(t *testing.T) {
	task := &model.Task{ID: "task-4", Description: "d"}
	bodyWithTombstone := []HistoryEntry{
		{ToolCalled: true, AssistantContent: "读文件",
			ToolCalls:   []llm.ToolCall{{ID: "c1", Name: "read_file", Arguments: map[string]any{"path": "a.go"}}},
			ToolResults: []ToolResult{{ToolCallID: "c1", Content: "[已清空] read_file a.go（原 500 字符）：内容已被历史压缩清理"}}},
	}

	// L1 墓碑 → snipped。
	ctx := WithAgentContext(context.Background(), "agent-1", "task-4", 3)
	m := buildContextManifest(ctx, "sys", task, nil, bodyWithTombstone, "", nil)
	if got := manifestItemByID(m, ManifestSectionHistory).Disposition; got != DispositionSnipped {
		t.Errorf("L1 墓碑应判 snipped, 实际 %q", got)
	}

	// L2 strategy 回填 → compressed:summary+keep_recent=3（strategy 一并记录）。
	ctx2, info2 := manifestTestSideCtx("task-4", 4)
	info2.l2Strategy = "summary+keep_recent=3"
	m2 := buildContextManifest(ctx2, "sys", task, nil, bodyWithTombstone, "", nil)
	if got := manifestItemByID(m2, ManifestSectionHistory).Disposition; got != "compressed:summary+keep_recent=3" {
		t.Errorf("L2 应判 compressed:summary+keep_recent=3, 实际 %q", got)
	}

	// L3 回填优先级最高 → truncated。
	ctx3, info3 := manifestTestSideCtx("task-4", 5)
	info3.l2Strategy = "summary+keep_recent=3"
	info3.l3Truncated = true
	m3 := buildContextManifest(ctx3, "sys", task, nil, bodyWithTombstone, "", nil)
	if got := manifestItemByID(m3, ManifestSectionHistory).Disposition; got != DispositionTruncated {
		t.Errorf("L3 应判 truncated, 实际 %q", got)
	}

	// 无压缩 → included。
	ctx4 := WithAgentContext(context.Background(), "agent-1", "task-4", 0)
	m4 := buildContextManifest(ctx4, "sys", task, nil, []HistoryEntry{{Output: "普通输出"}}, "", nil)
	if got := manifestItemByID(m4, ManifestSectionHistory).Disposition; got != DispositionIncluded {
		t.Errorf("无压缩应判 included, 实际 %q", got)
	}
}

// TestManifestSideInfo_MemoryFreshness 验证 memory 段 freshness 判定：
// UpdatedAt ≥ 任务开始 = live；早于 = stale；无记录/零值 = snapshot。
func TestManifestSideInfo_MemoryFreshness(t *testing.T) {
	start := time.Now()
	info := newManifestSideInfo(start)

	// 无记录 → snapshot。
	if got := info.memoryFreshness(markerTeamSnapshot); got != FreshnessSnapshot {
		t.Errorf("无记录应判 snapshot, 实际 %q", got)
	}
	// 零值时间不记录。
	info.recordMemorySectionUpdatedAt(markerTeamSnapshot, time.Time{})
	if got := info.memoryFreshness(markerTeamSnapshot); got != FreshnessSnapshot {
		t.Errorf("零值 UpdatedAt 应判 snapshot, 实际 %q", got)
	}
	// 任务开始之后 → live。
	info.recordMemorySectionUpdatedAt(markerTeamSnapshot, start.Add(time.Second))
	if got := info.memoryFreshness(markerTeamSnapshot); got != FreshnessLive {
		t.Errorf("任务开始后写入应判 live, 实际 %q", got)
	}
	// 早于任务开始 → stale。
	info.recordMemorySectionUpdatedAt(markerTeamSnapshot, start.Add(-time.Minute))
	if got := info.memoryFreshness(markerTeamSnapshot); got != FreshnessStale {
		t.Errorf("早于任务开始应判 stale, 实际 %q", got)
	}
	// nil 载体安全。
	var nilInfo *manifestSideInfo
	if got := nilInfo.memoryFreshness(markerTeamSnapshot); got != FreshnessSnapshot {
		t.Errorf("nil 侧信息应判 snapshot, 实际 %q", got)
	}
	nilInfo.recordMemorySectionUpdatedAt(markerTeamSnapshot, time.Now()) // 不 panic 即可
}

// TestContextManifest_SummaryJSON 验证 trace Description 摘要：只含元数据，
// 不含正文，字段齐全。
func TestContextManifest_SummaryJSON(t *testing.T) {
	b := NewManifestBuilder("task-1", 0)
	b.Register(ManifestSectionSystemPrompt, SourceAgentPrompt, ScopeProcess,
		AuthorityAuthoritative, FreshnessLive, DispositionIncluded, "秘密正文不应出现在摘要")
	m := b.Seal()

	summary := m.SummaryJSON()
	if strings.Contains(summary, "秘密正文") {
		t.Errorf("摘要不应含正文: %s", summary)
	}
	var items []manifestItemSummary
	if err := json.Unmarshal([]byte(summary), &items); err != nil {
		t.Fatalf("摘要应为合法 JSON: %v", err)
	}
	if len(items) != 1 || items[0].ID != ManifestSectionSystemPrompt ||
		items[0].Source != SourceAgentPrompt || items[0].Authority != AuthorityAuthoritative ||
		items[0].Freshness != FreshnessLive || items[0].Disposition != DispositionIncluded {
		t.Fatalf("摘要字段不正确: %s", summary)
	}
}

// --- processTask 端到端 trace 断言 ---

// TestProcessTask_ContextManifestTraceEvents 验证每轮 LLM 调用恰好一条
// context_manifest_built 事件、落任务分片、字段正确。
func TestProcessTask_ContextManifestTraceEvents(t *testing.T) {
	dir := captureTraceToDir(t)
	s, r, _ := setup()

	task := &model.Task{Description: "manifest trace e2e", EventType: "code"}
	if err := s.PublishTask(task); err != nil {
		t.Fatalf("PublishTask: %v", err)
	}
	if err := s.ClaimTask("agent-1", task.ID); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}

	// 两轮：第一轮调工具，第二轮纯文本完成。
	mock := &mockLLMClient{responses: []llm.Response{
		{ToolCalls: []llm.ToolCall{{ID: "c1", Name: "read_file", Arguments: map[string]any{"path": "a.go"}}}},
		{Content: "完成"},
	}}
	tools := NewToolRegistry()
	tools.Register("read_file", "读取文件", nil, func(ctx context.Context, args map[string]any) (string, error) {
		return "ok", nil
	})
	executor := NewLLMExecutor(mock, tools, nil, nil, nil, "", "系统提示")
	ag := NewAgent("agent-1", "code", s, r, executor)
	ag.processTask(context.Background(), task.ID)

	// 落任务分片：目录内应存在以 task_id 前 8 位命名的分片。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	shardFound := false
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") && strings.Contains(e.Name(), task.ID[:8]) {
			shardFound = true
		}
	}
	if !shardFound {
		t.Fatalf("未找到任务 %s 的 trace 分片", task.ID[:8])
	}

	var manifestEvents, llmStarts []trace.Event
	for _, ev := range readTraceEventsFromDir(t, dir) {
		switch ev.Kind {
		case trace.KindContextManifestBuilt:
			manifestEvents = append(manifestEvents, ev)
		case trace.KindLLMCallStart:
			llmStarts = append(llmStarts, ev)
		}
	}

	// 每轮恰好一条：与 llm_call_start 一一对应。
	if len(manifestEvents) != 2 || len(llmStarts) != 2 {
		t.Fatalf("context_manifest_built = %d 条, llm_call_start = %d 条, want 各 2",
			len(manifestEvents), len(llmStarts))
	}
	for i, ev := range manifestEvents {
		if ev.TaskID != task.ID {
			t.Errorf("事件 %d TaskID = %q, want %q", i, ev.TaskID, task.ID)
		}
		if ev.Loop != i {
			t.Errorf("事件 %d Loop = %d, want %d", i, ev.Loop, i)
		}
		if ev.AgentID != "agent-1" {
			t.Errorf("事件 %d AgentID = %q, want agent-1", i, ev.AgentID)
		}
		if ev.PromptTokens <= 0 {
			t.Errorf("事件 %d 估算 tokens = %d, want > 0", i, ev.PromptTokens)
		}
		// Description 是逐段 JSON 摘要：首轮必含 system_prompt / task_context /
		// task_desc / tools_schema。
		var items []manifestItemSummary
		if err := json.Unmarshal([]byte(ev.Description), &items); err != nil {
			t.Fatalf("事件 %d Description 非合法 JSON: %v", i, err)
		}
		ids := make(map[string]bool, len(items))
		for _, item := range items {
			ids[item.ID] = true
		}
		for _, wantID := range []string{ManifestSectionSystemPrompt, ManifestSectionTaskContext,
			ManifestSectionTaskDesc, ManifestSectionToolsSchema} {
			if !ids[wantID] {
				t.Errorf("事件 %d 缺少段 %s: %s", i, wantID, ev.Description)
			}
		}
	}
	// 第二轮的历史主体段应出现（第一轮的工具轮已进入 history）。
	var loop1Items []manifestItemSummary
	if err := json.Unmarshal([]byte(manifestEvents[1].Description), &loop1Items); err != nil {
		t.Fatalf("解析第二轮摘要失败: %v", err)
	}
	histFound := false
	for _, item := range loop1Items {
		if item.ID == ManifestSectionHistory {
			histFound = true
			if item.Count != 1 {
				t.Errorf("第二轮 history Count = %d, want 1", item.Count)
			}
			if item.Disposition != DispositionIncluded {
				t.Errorf("无压缩时 history Disposition = %q, want included", item.Disposition)
			}
		}
	}
	if !histFound {
		t.Errorf("第二轮应出现 history 主体段: %s", manifestEvents[1].Description)
	}
}
