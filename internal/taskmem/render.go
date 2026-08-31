// render.go 实现 Task Memory 的有界渲染（注入上下文用）。
//
// 段落优先级：当前 Observation 状态必须先于历史动作审计：
//
//	Goal/Constraints > Phase > Blockers > confirmed Facts > NextCandidates
//	> inferred Observation claims > Files > Failures（尾部）> Actions
//
// 预算执法：按优先级依次装填；列表段放不进剩余预算时从**最旧**条目开始
// 省略（保留最近），整段一条都放不下则跳过低优先级段。inferred Facts 使用
// 独立“待验证观察”标题，不能混入“已确认事实”或晋升为 Session 权威结论。
package taskmem

import (
	"fmt"
	"strings"
)

// Render 把 Task Memory 渲染为注入文本。budgetRunes > 0 时仅供显式测试或
// 上层调用者施加边界；<=0 表示完整渲染，由 L2 Context policy 按当前模型
// capability 统一决定 inline/reference，TaskMemory 不再私设 1500-rune 瓶颈。
func Render(m *TaskMemory, budgetRunes int) string {
	if m == nil {
		return ""
	}
	if budgetRunes <= 0 {
		budgetRunes = int(^uint(0) >> 2)
	}

	header := fmt.Sprintf("<task-memory source=\"task-memory\" version=\"%d\">\n", m.Version)
	footer := "</task-memory>"
	if runeLen(header)+runeLen(footer)+1 >= budgetRunes {
		// 预算极端小：保底硬截断（仍封闭标签，不产出半个 XML）。
		return truncateRunes(header+footer, budgetRunes)
	}
	remaining := budgetRunes - runeLen(header) - runeLen(footer)

	var body strings.Builder

	// 第 1 优先：目标与约束（不可整段丢弃；预算不足时硬截断 Goal）。
	goalText := "目标: " + m.Goal + "\n"
	if len(m.Constraints) > 0 {
		goalText += "约束: " + strings.Join(m.Constraints, "；") + "\n"
	}
	if runeLen(goalText) > remaining {
		goalText = truncateRunes(goalText, remaining)
	}
	body.WriteString(goalText)
	remaining -= runeLen(goalText)

	// 第 2 优先：阶段。
	phaseText := "阶段: " + m.Phase + "\n"
	if runeLen(phaseText) <= remaining {
		body.WriteString(phaseText)
		remaining -= runeLen(phaseText)
	}

	// 后续列表段按优先级装填。lines 按时间序（旧→新），省略从头部发生。
	sections := []struct {
		title string
		lines []string
	}{
		{"当前阻塞:", append([]string(nil), m.Blockers...)},
		{"已确认事实:", confirmedFactLines(m)},
		{"下一步候选:", append([]string(nil), m.NextCandidates...)},
		{"待验证观察:", inferredFactLines(m)},
		{"文件与产物版本:", fileLines(m)},
		{"失败尝试:", append([]string(nil), m.Failures...)},
		{"已完成动作:", actionLines(m)},
	}
	for _, sec := range sections {
		if len(sec.lines) == 0 {
			continue
		}
		text := renderListSection(sec.title, sec.lines, remaining)
		if text == "" {
			continue // 剩余预算放不下本段任何条目：跳过低优先级段
		}
		body.WriteString(text)
		remaining -= runeLen(text)
	}

	return header + body.String() + footer
}

// renderListSection 在剩余预算内渲染一个列表段：标题 + 尽量多的**最近**
// 条目；有省略时首行标注「…（略 N 条较早记录）」。连标题+一条都放不下
// 时返回空串（由调用方跳过本段）。
func renderListSection(title string, lines []string, budget int) string {
	if runeLen(title)+1 > budget {
		return ""
	}
	// 从最新向最旧装填，装满为止。
	kept := make([]string, 0, len(lines))
	used := runeLen(title) + 1
	for i := len(lines) - 1; i >= 0; i-- {
		line := "- " + lines[i] + "\n"
		if used+runeLen(line) > budget {
			break
		}
		kept = append([]string{lines[i]}, kept...)
		used += runeLen(line)
	}
	if len(kept) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(title + "\n")
	omitted := len(lines) - len(kept)
	if omitted > 0 {
		marker := fmt.Sprintf("- …（略 %d 条较早记录）\n", omitted)
		// 省略标记本身超预算时静默不标（条目截断已是充分信号）。
		if used+runeLen(marker) <= budget {
			sb.WriteString(marker)
		}
	}
	for _, l := range kept {
		sb.WriteString("- " + l + "\n")
	}
	return sb.String()
}

// actionLines 渲染动作条目（最近优先在 renderListSection 内处理，这里
// 保持时间序）。
func actionLines(m *TaskMemory) []string {
	lines := make([]string, 0, len(m.Actions))
	for _, a := range m.Actions {
		lines = append(lines, a.Caption)
	}
	return lines
}

// confirmedFactLines 只渲染 confirmed Facts；inferred 由独立低权威段处理。
func confirmedFactLines(m *TaskMemory) []string {
	lines := make([]string, 0, len(m.Facts))
	for _, f := range m.Facts {
		if f.Confirmed {
			lines = append(lines, f.Text)
		}
	}
	return lines
}

// inferredFactLines 把 evidence-bound 但未经语义核验的模型 claim 放在独立
// 低权威段；标题是 Context 契约的一部分，调用方不得去掉后与 confirmed 混合。
func inferredFactLines(m *TaskMemory) []string {
	lines := make([]string, 0, len(m.Facts))
	for _, fact := range m.Facts {
		if !fact.Confirmed {
			lines = append(lines, fact.Text)
		}
	}
	return lines
}

// fileLines 渲染文件版本（hash 取前 8 位缩写）。
func fileLines(m *TaskMemory) []string {
	lines := make([]string, 0, len(m.Files))
	for _, f := range m.Files {
		line := f.Path
		if f.Hash != "" {
			hash := f.Hash
			if len(hash) > 8 {
				hash = hash[:8]
			}
			line += " (hash:" + hash + ")"
		}
		lines = append(lines, line)
	}
	return lines
}

// runeLen 按 rune 计长（中文按 1 计，与 Manifest token 估算同口径）。
func runeLen(s string) int { return len([]rune(s)) }
