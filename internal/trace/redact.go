package trace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"sync/atomic"
)

// V6 §7.4 默认脱敏（schema-aware redaction）。
//
// trace 事件此前完整携带工具参数（write_file 正文、publish_task 描述、
// send_message 正文等自由内容全量落盘）。本文件提供发射处的统一脱敏入口：
// 事件落盘前对 Args / ShellExec.Command 过一次 RedactArgs / RedactShellCommand。
//
// 规则分三档：
//   - 结构字段原样保留：path / url / name / kind / event_type 等——Reactor
//     消费面（read-set-write 读 args.path、record-artifact 读 ShellExec.Outcome）
//     与 CLI/UI 展示不受影响；
//   - 截断保留字段：command / expected_artifacts 保留前 redactTextMaxRunes
//     字符并附截断标记（命令是排障高价值字段，但完整命令可能极长）；
//   - 自由内容字段：content（write_file 正文 / send_message 正文）、
//     old_str / new_str（edit_file 替换文本）、description（publish_task 描述）
//     一律替换为 <redacted len=N sha256=前12> 占位；
//   - 其余未列名字段：标量原样保留，超过 redactTextMaxRunes 字符的字符串
//     （或 JSON 序列化后超长的 map/slice）按同一占位式替换。
//
// 开发旁路：AGENTGO_TRACE_FULL_ARGS=1（bootstrap 读取后经 SetFullArgsEnabled
// 注入）整体关闭脱敏，与 AGENTGO_DUMP_PROMPTS 同级的显式开发开关。

// redactTextMaxRunes 是「长文本」判定阈值（字符数）：超过即脱敏或截断。
const redactTextMaxRunes = 200

var fullArgsEnabled atomic.Bool

// SetFullArgsEnabled 设置「完整参数记录」开关：true 时 RedactArgs /
// RedactShellCommand 原样返回，不做任何脱敏。仅由 bootstrap 读取
// AGENTGO_TRACE_FULL_ARGS 环境变量后注入，属开发调试通道。
func SetFullArgsEnabled(v bool) { fullArgsEnabled.Store(v) }

// FullArgsEnabled 报告完整参数记录开关是否打开。
func FullArgsEnabled() bool { return fullArgsEnabled.Load() }

// freeContentFields 是自由内容字段表：无论出现在哪个工具的参数里，字符串值
// 一律替换为 <redacted len=N sha256=前12> 占位。
var freeContentFields = map[string]bool{
	"content":     true, // write_file 正文 / send_message 正文
	"old_str":     true, // edit_file 被替换文本
	"new_str":     true, // edit_file 替换后文本
	"description": true, // publish_task 任务描述
}

// truncatedFields 是截断保留字段表：结构字段但可能超长，保留前
// redactTextMaxRunes 字符并附截断标记。
var truncatedFields = map[string]bool{
	"command":            true, // run_shell 命令
	"expected_artifacts": true, // publish_task 预期产物清单（逗号分隔）
}

// preservedFields 是结构字段表：无论多长都原样保留——路由/过滤/审计身份
// 字段，脱敏会砍断 Reactor 消费面（read-set-write 读 args.path）与排障定位。
var preservedFields = map[string]bool{
	"path":           true, // read/write/edit/grep/glob/list/probe 的目标路径
	"url":            true, // web_fetch 目标
	"name":           true, // 实体名（模板名、图节点名等）
	"kind":           true, // 类别标识
	"event_type":     true, // 任务路由类型
	"to":             true, // send_message 收件人
	"msg_type":       true, // 消息类型
	"priority":       true, // 优先级
	"status":         true, // submit_task_result 自述终态等枚举
	"verdict":        true, // 验收结论枚举
	"event":          true, // 结果事件名（Graph 边条件匹配键）
	"reason_code":    true, // 稳定原因码
	"urgency":        true, // replan 紧急度
	"isolation":      true, // 执行隔离声明
	"model":          true, // 模型覆盖
	"tools":          true, // 节点工具子集（逗号分隔）
	"working_dir":    true, // shell 工作目录
	"expected_hash":  true, // 乐观并发哈希
	"dependencies":   true, // 依赖任务 ID 清单
	"extract_mode":   true, // web_fetch 抽取模式
	"time_range":     true, // web_search 时间范围
	"pattern":        true, // grep/glob 匹配模式
	"parent_task_id": true, // 父任务 ID
	"batch_id":       true, // 批次 ID
}

// RedactArgs 返回工具参数的脱敏副本（绝不修改入参——原 map 还要继续参与
// 工具 dispatch 与 ToolCallRecord 账本）。tool 参数当前仅作语义标注与
// 将来按工具细化规则的挂点，现行规则按字段名驱动。
func RedactArgs(tool string, args map[string]any) map[string]any {
	_ = tool
	if len(args) == 0 {
		return args
	}
	if FullArgsEnabled() {
		return args
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		out[k] = redactArgValue(k, v)
	}
	return out
}

// RedactShellCommand 供 shell 工具事件发射处脱敏 ShellExec.Command：
// 保留前 redactTextMaxRunes 字符并附截断标记。
func RedactShellCommand(command string) string {
	if FullArgsEnabled() {
		return command
	}
	return truncateText(command, redactTextMaxRunes)
}

// redactArgValue 按字段名 + 值类型裁决单个参数值的脱敏方式。
func redactArgValue(key string, v any) any {
	if preservedFields[key] {
		return v
	}
	if freeContentFields[key] {
		if s, ok := v.(string); ok && s != "" {
			return redactedText(s)
		}
		return v
	}
	if truncatedFields[key] {
		if s, ok := v.(string); ok {
			return truncateText(s, redactTextMaxRunes)
		}
		return v
	}
	switch t := v.(type) {
	case string:
		if runeLen(t) > redactTextMaxRunes {
			return redactedText(t)
		}
		return t
	case map[string]any, []any:
		// 非标量值：JSON 序列化后按长文本同一阈值裁决，短的原样保留
		b, err := json.Marshal(t)
		if err != nil {
			return v
		}
		if runeLen(string(b)) > redactTextMaxRunes {
			return redactedText(string(b))
		}
		return v
	default:
		// 数字 / 布尔 / nil 等标量原样保留
		return v
	}
}

// redactedText 生成脱敏占位：<redacted len=N sha256=前12>。len 为字符数，
// sha256 取原文 UTF-8 字节的前 12 位十六进制——同原文同占位（digest 稳定），
// 可用于排障时比对两次调用是否携带同一内容。
func redactedText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "<redacted len=" + strconv.Itoa(runeLen(s)) + " sha256=" + hex.EncodeToString(sum[:])[:12] + ">"
}

// truncateText 保留前 maxRunes 字符并附截断标记；未超长的原样返回。
func truncateText(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "…[截断 len=" + strconv.Itoa(len(r)) + "]"
}

func runeLen(s string) int { return len([]rune(s)) }
