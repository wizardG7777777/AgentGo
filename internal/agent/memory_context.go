package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"agentgo/internal/memory"
	"agentgo/internal/roster"
)

// memory_context.go 是 v5 Phase 1 Memory System 的 Agent 侧读取/写入逻辑，
// 取代 v4 时代 internal/hook/builtin/team_awareness.go（MemoryManageSystem.md
// §4 迁移路径表）。
//
// 三个 section 的归属：
//   - TeamSnapshot：迁移到 Memory（key = "team_snapshot:<agentID>"）
//   - FileAwareness：迁移到 Memory（key = "file_awareness"，全局共享）
//   - GoalAnchor：直接删除（task.Description 已是目标，注入 GoalAnchor 是冗余）
//
// 写入侧策略：v5 Phase 1 采用 "Agent 主动 lazy compute + write-through cache"。
// MemoryManageSystem.md MM3/MM4 描述的"事件驱动写入"（Roster 监听 / scheduler
// 团队状态变更通知）需要更大架构改动，留作 v5.x。当前实现保持与 v4 等价的
// 计算成本（每 N 轮 compute 一次），但上下文流水通过 Memory 走，留好 v5.x
// 升级路径。

// memoryRefreshIntervalDefault 是默认的 team snapshot 刷新间隔（轮数），
// 与 v4 TeamAwarenessConfig.SnapshotRefreshInterval 保持一致。
const memoryRefreshIntervalDefault = 5

// teamSnapshotKey 构造 per-agent team_snapshot 在 Memory 中的检索键。
//
// 为什么 per-agent：BuildTeamSnapshot(selfID, ...) 返回的内容是"selfID 视角下
// 的其他 agent 状态"——不同 agent 看到的快照内容不同，必须按 agent 分键存储。
func teamSnapshotKey(agentID string) string {
	return "team_snapshot:" + agentID
}

// fileAwarenessKey 是文件占用快照的固定键。
//
// 为什么 singleton：file_awareness 内容是"全队所有 agent 的文件占用列表"
// （renderFileAwareness 中 self 标 "你"、其他标 "队友"），但底层数据
// （roster.ListClaims）对所有 agent 一致——存一份即可，渲染时 self 由调用方
// 传入决定。但 v5 Phase 1 简化为 "渲染后存"，per-agent 视角差异通过 key 加
// agentID 后缀承载。
func fileAwarenessKey(agentID string) string {
	return "file_awareness:" + agentID
}

// injectMemoryContext 是 v5 Phase 1 替代 PhaseTaskStart / PhaseLoopPre hook 注入
// 的主入口。返回拼接后的 IncomingMail 内容，由调用方追加到 history。
//
// loopIdx 语义：
//   - -1：任务入口（处理重试任务时跳过，与 v4 RetryCount > 0 短路一致）
//   - 0：首轮（首轮 inject 由 loopIdx=-1 路径承担，本路径返回 ""）
//   - >0：每 N 轮刷新（refreshInterval），或收到新邮件时强刷
//
// hasNewMail 为 true 时强制刷新 team_snapshot（与 v4 ForceOnMail=true 一致）。
//
// nil-safe：a.Memory 为 nil 时直接返回 ""，等价于禁用本特性。
func (a *Agent) injectMemoryContext(ctx context.Context, taskID string, loopIdx int, hasNewMail bool) string {
	if a.Memory == nil {
		return ""
	}

	// 重试任务在 TaskStart 阶段跳过 —— LastHistory 已含上次任务结束时的
	// 快照，重复注入会让 LLM 看到带误导性时间戳的旧数据。
	if loopIdx == -1 && a.Store != nil {
		if task, gerr := a.Store.GetTask(taskID); gerr == nil && task != nil && task.RetryCount > 0 {
			return ""
		}
	}

	// 首轮以外，若不到刷新点也不强制刷新，则 loopIdx>0 路径直接返回 ""——
	// 由 TaskStart 阶段（loopIdx=-1）已经注入过，避免每轮重复。
	refresh := loopIdx == -1 || hasNewMail
	if loopIdx > 0 {
		interval := a.TeamRefreshInterval
		if interval <= 0 {
			interval = memoryRefreshIntervalDefault
		}
		if loopIdx%interval == 0 {
			refresh = true
		}
	}
	if loopIdx == 0 {
		// 首轮注入由 TaskStart 路径（loopIdx=-1）承担，避免双重
		return ""
	}
	if !refresh {
		return ""
	}

	// TeamSnapshot：lazy compute + write-through cache
	if a.MailRegistry != nil && a.Store != nil {
		snapshot := BuildTeamSnapshot(a.ID, a.Store, a.MailRegistry)
		if snapshot != "" {
			_ = a.Memory.Put(ctx, memory.Entry{
				Scope:   memory.ScopeProcess,
				Kind:    memory.KindContext,
				Key:     teamSnapshotKey(a.ID),
				Content: snapshot,
				Source:  a.ID,
				Tags:    []string{"team_snapshot"},
			})
		}
	}

	// FileAwareness：同样 lazy compute + write-through。
	// Roster 为 nil 时退化为不输出（与 v4 行为一致）。
	if a.Roster != nil {
		fileAwareness := renderFileAwareness(a.ID, a.Roster)
		if fileAwareness != "" {
			_ = a.Memory.Put(ctx, memory.Entry{
				Scope:   memory.ScopeProcess,
				Kind:    memory.KindContext,
				Key:     fileAwarenessKey(a.ID),
				Content: fileAwareness,
				Source:  a.ID,
				Tags:    []string{"file_awareness"},
			})
		}
	}

	snapshot := a.queryMemoryContext(ctx, teamSnapshotKey(a.ID), "team_snapshot")
	fileAwareness := a.queryMemoryContext(ctx, fileAwarenessKey(a.ID), "file_awareness")
	return joinSections(snapshot, fileAwareness)
}

func (a *Agent) queryMemoryContext(ctx context.Context, keys ...string) string {
	for _, key := range keys {
		entries, err := a.Memory.Query(ctx, memory.ScopeProcess, memory.KindContext, key, 1)
		if err != nil || len(entries) == 0 {
			continue
		}
		if content := strings.TrimSpace(entries[0].Content); content != "" {
			return content
		}
	}
	return ""
}

// joinSections 用空行拼接非空段。与 hook/builtin/team_awareness.go 同型函数语义一致。
func joinSections(parts ...string) string {
	nonEmpty := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			nonEmpty = append(nonEmpty, s)
		}
	}
	return strings.Join(nonEmpty, "\n\n")
}

// renderFileAwareness 读取 Roster 文件占用快照，按 agent 分组渲染。
// 当前 agent 用 "你（<id>）已占用" 前缀，其他用 "<id> 正在修改"。
// 排序保持稳定输出（便于测试断言）。
//
// 从 hook/builtin/team_awareness.go renderFileAwareness 平移过来——v5 Phase 1
// 把这块算力从 hook 上下文挪进 agent 主流程，逻辑保持字节级一致。
func renderFileAwareness(selfID string, r roster.Roster) string {
	if r == nil {
		return ""
	}
	claims := r.ListClaims()
	if len(claims) == 0 {
		return ""
	}

	ids := make([]string, 0, len(claims))
	for id := range claims {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var sb strings.Builder
	sb.WriteString("<file-awareness>\n")
	for _, agentID := range ids {
		files := claims[agentID]
		if len(files) == 0 {
			continue
		}
		sorted := make([]string, len(files))
		copy(sorted, files)
		sort.Strings(sorted)
		if agentID == selfID {
			fmt.Fprintf(&sb, "  - 你（%s）已占用: %s\n", agentID, strings.Join(sorted, ", "))
		} else {
			fmt.Fprintf(&sb, "  - %s 正在修改: %s\n", agentID, strings.Join(sorted, ", "))
		}
	}
	sb.WriteString("</file-awareness>")
	return sb.String()
}
