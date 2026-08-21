package store

// 本文件是工具调用账本（ToolCallRecord）的工具名清洗与合法性判定（SWE-002
// 第二层防线，同时关闭 SWE-003 残余①）：
//
// 模型可能往 tool_call 的名字段泄漏 DSML 标记，产生 200+ 字符、含控制字符/
// 空白/< > | 等标记字符的畸形「工具名」。AppendToolCall 是账本唯一写入点，
// 在持久化前把畸形名替换为确定性占位，下游一切以工具名为键的逻辑
// （TaskMemory 身份、graph evidence、anomaly 启发式）自然吃到干净名。
// 原始垃圾名不进账本——trace 的工具调用事件仍保留原始 ToolCall 可对账。

import (
	"crypto/sha256"
	"fmt"
	"log"
	"regexp"
	"unicode/utf8"
)

// toolNameMaxRunes 是合法工具名的长度上限（与 graph evidence kind 归一
// 同口径，足以容纳全部内置工具名）。
const toolNameMaxRunes = 64

// toolNameCharsetPattern 是合法工具名字符集：字母开头，其余仅限字母数字与
// . _ : -。含控制字符、空白、< > | 等标记字符或 CJK 的名字一律非法。
var toolNameCharsetPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:-]*$`)

// IsToolNameCharsetLegal 判定工具名的字符形状是否合法（非空、字母开头、
// 字符集内），不检查长度。evidence kind 归一用它区分「形状非法 → unknown」
// 与「形状合法但超长 → 截断」。
func IsToolNameCharsetLegal(name string) bool {
	return name != "" && toolNameCharsetPattern.MatchString(name)
}

// IsWellFormedToolName 判定工具名是否完整合法：字符形状合法且 ≤ 64 rune。
// 落库清洗与 evidence tool_name 归一以本规则为准。
func IsWellFormedToolName(name string) bool {
	return IsToolNameCharsetLegal(name) && utf8.RuneCountInString(name) <= toolNameMaxRunes
}

// MalformedToolNamePlaceholder 为畸形工具名生成确定性占位
// malformed:<sha256(raw) 前 12 hex>：同一垃圾名永远同一占位，不同垃圾名
// 可区分。占位本身满足 IsWellFormedToolName（字母开头、22 字符、字符集内），
// 下游二次校验不会再次命中清洗。
func MalformedToolNamePlaceholder(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("malformed:%x", sum[:6])
}

// sanitizeToolNameForLedger 在 AppendToolCall 持锁路径上清洗工具名：合法名
// 逐字节不动；畸形名替换为确定性占位并告警。告警按 store 实例对同一占位
// 去重（生产单实例即进程级一次）；日志只带长度不带原始内容，避免垃圾名里
// 的换行/控制字符污染日志。
func (s *MemoryTaskStore) sanitizeToolNameForLedger(taskID, name string) string {
	if IsWellFormedToolName(name) {
		return name
	}
	placeholder := MalformedToolNamePlaceholder(name)
	if s.malformedToolNames == nil {
		s.malformedToolNames = make(map[string]struct{})
	}
	if _, warned := s.malformedToolNames[placeholder]; !warned {
		s.malformedToolNames[placeholder] = struct{}{}
		log.Printf("[store] WARN 任务 %s 的工具调用记录含畸形工具名（%d rune），已清洗为 %q；原始名可从 trace 工具调用事件对账",
			taskID, utf8.RuneCountInString(name), placeholder)
	}
	return placeholder
}
