// Package prompt 实现 V6 §2（P1a）的 Prompt 有序编译：把进入 LLM 上下文的
// system prompt 与首条 user 消息按稳定组件序列编译为一份带身份（Build.ID）
// 的冻结构建产物。
//
// P1a 阶段编译产物只用于身份与观测（trace 账本 / 审计），不改变任何消息
// 字节——渲染路径仍由 agent.buildMessages 承担；Build.Text 与其
// system+user 首条逐字节一致由 agent 侧测试钉住。
//
// 身份规则：
//   - Component.Digest = 组件 Text 的 sha256 前 12（Compile 统一计算，调用方
//     预置值一律被覆盖，保证身份与正文一致）；
//   - Build.Text = InMessage 组件正文按参数序拼接；
//   - Build.Digest = Build.Text 的 sha256 前 12；
//   - Build.ID = "pb-" + 全链 digest（逐组件 "ID@Version:Digest" 链的
//     sha256 前 12）——任一组件的来源版本或正文变化都会改变 Build.ID，
//     输入不变则 Build.ID 跨 attempt / 跨进程稳定。
package prompt

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// 组件 ID 词表（稳定标识，trace 摘要与测试断言共用）。
const (
	// ComponentBaseContract 是基础契约段（如团队能力感知 + 纪律提醒，
	// 启动期冻结的静态文本，注入 user 首条最前）。
	ComponentBaseContract = "base_contract"
	// ComponentAgentRole 是 agent 角色段（system prompt 全文，system 消息）。
	ComponentAgentRole = "agent_role"
	// ComponentTaskObjective 是任务目标段（任务描述 + 前置任务结果）。
	ComponentTaskObjective = "task_objective"
	// ComponentControlProtocol 是控制协议段（<task-context> 控制面块）。
	ComponentControlProtocol = "control_protocol"
	// ComponentSafetyNotice 是动态权限/边界段（预留；当前无运行时注入点）。
	ComponentSafetyNotice = "safety_notice"
	// ComponentToolGuidance 是工具指引段（当时工具清单摘要）。工具定义经
	// LLM API 的 tools 协议通道下发而非消息字节，故为带外身份组件。
	ComponentToolGuidance = "tool_guidance"
	// ComponentOutputContract 是结果协议段（任务结果提交协议简述）。
	// 协议正文由工具 schema 与系统内生通道承担，故为带外身份组件。
	ComponentOutputContract = "output_contract"
)

// buildIDChainPrefix 是 Build.ID 全链 digest 的域分隔前缀；链格式变更时递增。
const buildIDChainPrefix = "prompt-build/v1\n"

// Component 是 Prompt 构建中的一个有序组件。ID/Version 由调用方按来源
// 填充；Digest 由 Compile 统一计算。
type Component struct {
	ID      string // 稳定组件 ID（见上方词表）
	Version string // 来源版本（文件 digest / 模板 Version / 协议版本）
	Digest  string // 组件身份 digest（Compile 按 Text 计算：sha256 前 12）
	Text    string // 组件正文
	// InMessage 标记组件正文是否进入 LLM 消息字节（true=消息绑定组件，进入
	// Build.Text；false=带外身份组件，只参与身份与观测）。
	InMessage bool
}

// Build 是一次 Prompt 编译的冻结产物。同一 attempt 的各轮 LLM 调用复用
// 同一 Build（冻结纪律：核心指令执行中不改变）。
type Build struct {
	ID         string      // prompt_build_id："pb-" + 全链 digest
	Components []Component // 顺序即编译顺序
	Text       string      // InMessage 组件正文按序拼接
	Digest     string      // Text 的 sha256 前 12
}

// DigestText 计算内容 sha256 的前 12 位 hex（与 trace / Context Manifest
// 的 digest 口径一致）。
func DigestText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

// Compile 按参数顺序编译组件序列（顺序即身份的一部分）：逐组件计算
// Digest（覆盖调用方预置值），拼接 InMessage 正文为 Build.Text，并以
// 「ID@Version:Digest」链计算 Build.ID。空组件序列是合法输入（ID 仍稳定）。
func Compile(parts []Component) Build {
	comps := make([]Component, len(parts))
	var text strings.Builder
	var chain strings.Builder
	chain.WriteString(buildIDChainPrefix)
	for i, p := range parts {
		p.Digest = DigestText(p.Text)
		comps[i] = p
		chain.WriteString(p.ID)
		chain.WriteString("@")
		chain.WriteString(p.Version)
		chain.WriteString(":")
		chain.WriteString(p.Digest)
		chain.WriteString("\n")
		if p.InMessage {
			text.WriteString(p.Text)
		}
	}
	t := text.String()
	return Build{
		ID:         "pb-" + DigestText(chain.String()),
		Components: comps,
		Text:       t,
		Digest:     DigestText(t),
	}
}

// ComponentSummary 是 trace 事件 Description 承载的单组件身份摘要（不含正文）。
type ComponentSummary struct {
	ID        string `json:"id"`
	Version   string `json:"version,omitempty"`
	Digest    string `json:"digest"`
	InMessage bool   `json:"in_message"`
}

// ComponentsSummaryJSON 生成 prompt_compiled 事件 Description 用的紧凑
// JSON：逐组件 ID/Version/Digest/InMessage，不含任何正文（与 Context
// Manifest 同一脱敏纪律）。
func (b Build) ComponentsSummaryJSON() string {
	summaries := make([]ComponentSummary, 0, len(b.Components))
	for _, c := range b.Components {
		summaries = append(summaries, ComponentSummary{
			ID:        c.ID,
			Version:   c.Version,
			Digest:    c.Digest,
			InMessage: c.InMessage,
		})
	}
	data, err := json.Marshal(summaries)
	if err != nil {
		return "[]"
	}
	return string(data)
}
