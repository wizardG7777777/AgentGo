package agent

import (
	"agentgo/internal/memory"
	"agentgo/internal/roster"
	"context"
	"fmt"
	"sort"
	"strings"
)

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

func (a *Agent) refreshRuntimeFacts(ctx context.Context, taskID string, loopIdx int, hasNewMail bool) {
	if a.Memory == nil {
		return
	}

	// 重试任务在 TaskStart 阶段跳过 —— LastHistory 已含上次任务结束时的
	// 快照，重复注入会让 LLM 看到带误导性时间戳的旧数据。
	if loopIdx == -1 && a.Store != nil {
		if task, gerr := a.Store.GetTask(taskID); gerr == nil && task != nil && task.RetryCount > 0 {
			return
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
		return
	}
	if !refresh {
		return
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

}
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
