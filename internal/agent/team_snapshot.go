package agent

import (
	"fmt"
	"strings"

	"agentgo/internal/mailbox"
	"agentgo/internal/model"
	"agentgo/internal/store"
)

// BuildTeamSnapshot 构建当前团队状态快照文本，注入代理的 LLM 上下文。
// 包含：当前活跃代理列表、各代理正在执行的任务摘要。
//
// 该函数从 internal/worker.BuildTeamSnapshot 迁移而来（v4 §11.6.6 计划：
// worker/explorer 折叠为统一 runner 后，此辅助函数归位 internal/agent）。
// 当前 worker 包仍持有副本——bootstrap 完成 v4 切换 + worker/explorer 删除后，
// worker 副本同步移除。
//
// 调用方（bootstrap.go）通过闭包包装本函数注入 TeamAwarenessHook：
//
//	taCfg := builtin.TeamAwarenessConfig{
//	    SnapshotFn: func(selfID string) string {
//	        return agent.BuildTeamSnapshot(selfID, taskStore, mbRegistry)
//	    },
//	    ...
//	}
func BuildTeamSnapshot(selfID string, s store.TaskStore, mbRegistry *mailbox.Registry) string {
	tasks, err := s.ScanAll()
	if err != nil {
		return ""
	}

	type peerInfo struct {
		agentID  string
		taskDesc string
	}
	var peers []peerInfo
	for _, t := range tasks {
		if t.Status != model.TaskStatusProcessing {
			continue
		}
		for _, aid := range t.Agents {
			if aid == selfID {
				continue
			}
			desc := t.Description
			if len([]rune(desc)) > 80 {
				desc = string([]rune(desc)[:80]) + "..."
			}
			peers = append(peers, peerInfo{agentID: aid, taskDesc: desc})
		}
	}

	var idleIDs []string
	if mbRegistry != nil {
		busySet := make(map[string]bool)
		for _, p := range peers {
			busySet[p.agentID] = true
		}
		busySet[selfID] = true
		for _, id := range mbRegistry.AllIDs() {
			if !busySet[id] && id != "scheduler" {
				idleIDs = append(idleIDs, id)
			}
		}
	}

	if len(peers) == 0 && len(idleIDs) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("<team-snapshot>\n")
	sb.WriteString("以下是当前团队中其他代理的状态，你可以通过 send_message 工具直接联系他们：\n")
	for _, p := range peers {
		fmt.Fprintf(&sb, "  - %s [忙碌] 正在执行: %s\n", p.agentID, p.taskDesc)
	}
	for _, id := range idleIDs {
		fmt.Fprintf(&sb, "  - %s [空闲]\n", id)
	}
	sb.WriteString("</team-snapshot>")
	return sb.String()
}
