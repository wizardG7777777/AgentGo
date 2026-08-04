package model

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ExecutionLease 是 V6 §4（H1）引入的「冻结执行租约」：任务首次被认领时，
// 按 Lease = NodeRequirement ∩ RouteCeiling ∩ Policy 计算出的当次执行契约，
// 一经冻结不再随重试/恢复重新计算。
//
// 语义要点：
//   - NodeRequirement：task.Capability.Tools 显式声明即用；未声明走「合成节点
//     能力」规则（Synthetic=true）——需求合成为认领方 Route 工具白名单全量。
//     这是文档化的合成授予，取代旧「隐式继承 kind 全集」语义：旧语义里无
//     Capability 的任务根本不形成契约，新语义把同样的工具面落成可审计的租约。
//   - RouteCeiling：认领 Runner 的工具注册全集（rt.AllowedTools 装配产物）。
//   - Policy：exec=readonly 从 BusinessTools 剔除写工具（write_file /
//     edit_file / run_shell）；exec=strict 保留工具但记 ApprovalRequired=true
//     （逐次审批语义不变）；控制通道按节点角色派生（Graph agent 节点 =
//     {submit_task_result, request_replan}，非图执行任务 = {submit_task_result}，
//     scheduler 控制面任务 = {report_done}）。
//
// 生命周期：首次认领冻结（execution_lease_frozen）→ RetryRollback 后重认领
// 复用（execution_lease_reused，Digest 与工具面不变）→ 任务终态（含
// finalizing 被接受）撤销（execution_lease_revoked，Revoked=true，此后任何
// 工具 dispatch 拒绝——与 finalizing fence 互补的防御层）。
type ExecutionLease struct {
	TaskID   string    `json:"task_id"`
	Attempt  int       `json:"attempt"`   // 冻结时的执行尝试序号（1-based，= 冻结时 RetryCount+1）
	FrozenAt time.Time `json:"frozen_at"` // 首次冻结时刻（UTC）

	// BusinessTools 是精确业务工具集（交集结果，排序去重）。nil 表示「无裁剪
	// 面」——仅用于无工具换入面的控制面 agent（scheduler），执行侧不换入
	// 过滤视图。
	BusinessTools []string `json:"business_tools,omitempty"`
	// ControlTools 是节点角色派生的控制通道（排序去重）。生效工具视图为
	// BusinessTools ∪ ControlTools——显式声明漏带控制工具时节点仍能收尾。
	ControlTools []string `json:"control_tools,omitempty"`

	Model     string `json:"model,omitempty"`     // 冻结模型（capability 覆盖或 kind 默认）
	Workspace string `json:"workspace,omitempty"` // "" | "workspace"（写时复制执行隔离）

	Synthetic bool `json:"synthetic,omitempty"` // true = 需求为合成（未显式声明 tools）
	// ApprovalRequired 为 true 表示冻结时 exec=strict：写工具/shell 保留在
	// 工具面内、由既有审批装配逐次审批（语义不变，仅记账）。
	ApprovalRequired bool `json:"approval_required,omitempty"`

	Revoked bool `json:"revoked,omitempty"` // 终态/finalizing 被接受后置 true
	// Digest 是执行语义字段的稳定摘要（sha256 hex 前 12 字符）：覆盖
	// BusinessTools / ControlTools / Model / Workspace / Synthetic /
	// ApprovalRequired，不含 TaskID/Attempt/FrozenAt/Revoked 等生命周期
	// 元数据——同一冻结契约跨重试复用时 Digest 保持不变。
	Digest string `json:"digest,omitempty"`
}

// ComputeDigest 计算并返回租约执行语义字段的稳定摘要（sha256 hex 前 12）。
// 输入字段必须先排序去重（冻结路径由 compute 保证；手工构造时调用方负责）。
func (l *ExecutionLease) ComputeDigest() string {
	h := sha256.New()
	fmt.Fprintf(h, "biz=%s;ctl=%s;model=%s;ws=%s;syn=%t;appr=%t",
		strings.Join(l.BusinessTools, ","),
		strings.Join(l.ControlTools, ","),
		l.Model, l.Workspace, l.Synthetic, l.ApprovalRequired)
	sum := h.Sum(nil)
	return hex.EncodeToString(sum)[:12]
}

// ToolUnion 返回生效工具视图（BusinessTools ∪ ControlTools，排序去重）。
// 视图中的名字若未在认领方 registry 注册，Filtered 会静默跳过——并集只
// 承诺「注册过的名字不会被裁掉」，不承诺注册缺失的工具。
func (l *ExecutionLease) ToolUnion() []string {
	return SortedUnion(l.BusinessTools, l.ControlTools)
}

// SortedUnion 返回两个工具名清单的并集（排序去重）。供 Lease 计算与
// ToolUnion 共用。
func SortedUnion(a, b []string) []string {
	set := make(map[string]struct{}, len(a)+len(b))
	for _, name := range a {
		set[name] = struct{}{}
	}
	for _, name := range b {
		set[name] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// SortedCopy 返回工具名清单的排序去重副本；空输入返回 nil。
func SortedCopy(src []string) []string {
	if len(src) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(src))
	for _, name := range src {
		set[name] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
