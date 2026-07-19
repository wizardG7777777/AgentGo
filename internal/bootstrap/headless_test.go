package bootstrap

import "testing"

// TestShouldStartTUI frontends → 是否启动 TUI 的判定矩阵：
// 空列表按 config.validateUI 的默认回落视为 [tui]；含 "tui" 即启动；
// 仅 "web" 时进入 headless 模式。
func TestShouldStartTUI(t *testing.T) {
	cases := []struct {
		name      string
		frontends []string
		want      bool
	}{
		{"nil 默认启动 TUI", nil, true},
		{"空切片默认启动 TUI", []string{}, true},
		{"仅 tui", []string{"tui"}, true},
		{"仅 web 为 headless", []string{"web"}, false},
		{"web+tui 双前端", []string{"web", "tui"}, true},
		{"tui+web 双前端", []string{"tui", "web"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldStartTUI(c.frontends); got != c.want {
				t.Fatalf("shouldStartTUI(%v) = %v，期望 %v", c.frontends, got, c.want)
			}
		})
	}
}
