package taskmem

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestRender_BudgetRespected：渲染永不超预算；高优先级当前状态
// （目标/阶段/下一步）在预算紧张时先于历史动作保留。
func TestRender_BudgetRespected(t *testing.T) {
	m := New("task-1")
	m.Goal = "实现一个非常重要的功能，需要多步完成"
	m.Constraints = []string{"工具子集: read_file,write_file"}
	for i := 0; i < 15; i++ {
		m.Actions = append(m.Actions, ActionRecord{Caption: fmt.Sprintf("write_file 产出文件编号%02d.go", i)})
	}
	for i := 0; i < 8; i++ {
		m.Failures = append(m.Failures, fmt.Sprintf("web_fetch 调用失败: 地址编号%02d — 超时", i))
	}
	m.NextCandidates = []string{"候选甲", "候选乙"}

	for _, budget := range []int{1500, 400, 200} {
		out := Render(m, budget)
		if got := len([]rune(out)); got > budget {
			t.Fatalf("budget=%d 时渲染 %d runes 超预算", budget, got)
		}
		if !strings.HasPrefix(out, "<task-memory") || !strings.HasSuffix(out, "</task-memory>") {
			t.Fatalf("budget=%d 时渲染未封闭标签: %q", budget, out)
		}
		if !strings.Contains(out, "目标:") {
			t.Errorf("budget=%d 时目标段必须保留: %q", budget, out)
		}
	}
	// 充足预算下各段齐全。
	full := Render(m, 1500)
	for _, want := range []string{"目标:", "约束:", "阶段:", "已完成动作:", "失败尝试:", "下一步候选:"} {
		if !strings.Contains(full, want) {
			t.Errorf("充足预算下缺少段 %q: %q", want, full)
		}
	}
	// 紧张预算下历史动作不得抢占当前下一步。
	tight := Render(m, 200)
	if strings.Contains(tight, "已完成动作:") && !strings.Contains(tight, "下一步候选:") {
		t.Errorf("紧张预算下历史动作不应抢占当前状态: %q", tight)
	}
}

func TestRenderWithoutLocalBudgetIncludesCompleteBoundedState(t *testing.T) {
	m := New("task-complete")
	m.Goal = strings.Repeat("目标", 1000)
	for i := 0; i < MaxActions; i++ {
		m.Actions = append(m.Actions, ActionRecord{Caption: fmt.Sprintf("read_file-%02d", i)})
	}
	m.NextCandidates = []string{"实施修复", "运行 typed check"}
	out := Render(m, 0)
	if !strings.Contains(out, strings.Repeat("目标", 1000)) ||
		!strings.Contains(out, "实施修复") || !strings.Contains(out, "read_file-00") {
		t.Fatalf("无本地预算时必须完整渲染结构上已有状态: len=%d", len([]rune(out)))
	}
}

// TestRender_ListSectionKeepsRecent：列表段预算不足时保留最近条目，
// 省略最旧并标注。
func TestRender_ListSectionKeepsRecent(t *testing.T) {
	m := New("task-1")
	m.Goal = "g"
	for i := 0; i < 20; i++ {
		m.Actions = append(m.Actions, ActionRecord{Caption: fmt.Sprintf("write_file f%02d.go", i)})
	}
	out := Render(m, 300)
	if !strings.Contains(out, "f19.go") {
		t.Errorf("最近条目必须保留: %q", out)
	}
	if strings.Contains(out, "f00.go") && !strings.Contains(out, "略") {
		t.Errorf("省略最旧条目时应标注「略 N 条」: %q", out)
	}
}

// TestRender_InferredFactsUseExplicitLowAuthoritySection：inferred Facts 只能
// 进入显式低权威段，不能伪装成已确认事实。
func TestRender_InferredFactsUseExplicitLowAuthoritySection(t *testing.T) {
	m := New("task-1")
	m.Goal = "g"
	m.Facts = []Fact{
		{Text: "模型声称测试通过", Confirmed: false, UpdatedAt: time.Now()},
		{Text: "用户决定: 走方案 B", Confirmed: true, UpdatedAt: time.Now()},
	}
	out := Render(m, 1500)
	if !strings.Contains(out, "待验证观察:\n- 模型声称测试通过") {
		t.Errorf("inferred 事实必须带低权威标题渲染: %q", out)
	}
	if !strings.Contains(out, "用户决定: 走方案 B") {
		t.Errorf("confirmed 事实应渲染: %q", out)
	}
	if strings.Contains(out, "已确认事实:\n- 模型声称测试通过") {
		t.Errorf("inferred 事实不得混入 confirmed 段: %q", out)
	}
}

// TestRender_FileHashAbbreviated：文件版本 hash 缩写展示。
func TestRender_FileHashAbbreviated(t *testing.T) {
	m := New("task-1")
	m.Goal = "g"
	m.Files = []FileVersion{{Path: "a.go", Hash: "0123456789abcdef", UpdatedAt: time.Now()}}
	out := Render(m, 1500)
	if !strings.Contains(out, "a.go (hash:01234567)") {
		t.Errorf("文件版本应带 hash 缩写: %q", out)
	}
	if strings.Contains(out, "0123456789abcdef") {
		t.Errorf("完整 hash 不应渲染: %q", out)
	}
}

// TestRender_NilAndExtremeBudget：nil 与极端预算的健壮性。
func TestRender_NilAndExtremeBudget(t *testing.T) {
	if got := Render(nil, 100); got != "" {
		t.Errorf("nil 渲染应为空串, got %q", got)
	}
	m := New("task-1")
	out := Render(m, 20) // 预算小于 header+footer
	if len([]rune(out)) > 20 {
		t.Errorf("极端预算下渲染超预算: %d runes", len([]rune(out)))
	}
}
