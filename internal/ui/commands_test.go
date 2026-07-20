package ui

import (
	"strings"
	"testing"
)

// 命令目录是全部前端（TUI 提示框 / WebUI 补全 / /help）的单一数据源，
// 这里的守卫防止目录自身腐化：重名、空说明、非法 scope 都会直接破坏
// 两个前端的命令提示与解析。

func TestCommandCatalog_NamesAndAliasesUnique(t *testing.T) {
	seen := map[string]string{}
	for _, c := range CommandCatalog() {
		if c.Name == "" {
			t.Fatal("存在无名命令")
		}
		if prev, dup := seen[c.Name]; dup {
			t.Fatalf("命令名 %q 与 %q 重复", c.Name, prev)
		}
		seen[c.Name] = c.Name
		for _, a := range c.Aliases {
			if prev, dup := seen[a]; dup {
				t.Fatalf("别名 %q（属于 /%s）与 %q 冲突", a, c.Name, prev)
			}
			seen[a] = "alias of " + c.Name
		}
	}
}

func TestCommandCatalog_FieldsWellFormed(t *testing.T) {
	for _, c := range CommandCatalog() {
		if strings.TrimSpace(c.Desc) == "" {
			t.Fatalf("/%s 缺少说明文字", c.Name)
		}
		if c.Scope != ScopeShared && c.Scope != ScopeTUI {
			t.Fatalf("/%s 的 scope 非法: %q", c.Name, c.Scope)
		}
		if strings.Contains(c.Name, "/") || strings.ContainsAny(c.Name, " \t") {
			t.Fatalf("命令名含非法字符: %q", c.Name)
		}
	}
}

func TestCommandCatalog_UsageFormat(t *testing.T) {
	for _, c := range CommandCatalog() {
		u := c.Usage()
		if !strings.HasPrefix(u, "/"+c.Name) {
			t.Fatalf("Usage 应以 /%s 开头: %q", c.Name, u)
		}
		if c.Args == "" && u != "/"+c.Name {
			t.Fatalf("无参命令 Usage 不应带参数: %q", u)
		}
		if c.Args != "" && !strings.HasSuffix(u, c.Args) {
			t.Fatalf("Usage 未携带参数形态 %q: %q", c.Args, u)
		}
	}
}

// 两个前端都依赖这组 shared 命令（WebUI 只暴露 shared 子集），
// 缺任何一个都是跨端能力缺口。
func TestCommandCatalog_SharedSetComplete(t *testing.T) {
	want := []string{"help", "status", "cancel", "mode", "steer", "new", "session"}
	for _, name := range want {
		c, ok := MatchCommand(name)
		if !ok {
			t.Fatalf("目录缺少 shared 命令 /%s", name)
		}
		if c.Scope != ScopeShared {
			t.Fatalf("/%s 应为 ScopeShared，实际 %q", name, c.Scope)
		}
	}
}

func TestCommandCatalog_LayeredTUIViewsComplete(t *testing.T) {
	for _, name := range []string{"activity", "logs", "trace"} {
		c, ok := MatchCommand(name)
		if !ok {
			t.Fatalf("目录缺少分层视图命令 /%s", name)
		}
		if c.Scope != ScopeTUI {
			t.Fatalf("/%s 应为 ScopeTUI，实际 %q", name, c.Scope)
		}
	}
}

func TestMatchCommand_Alias(t *testing.T) {
	c, ok := MatchCommand("dash")
	if !ok || c.Name != "dashboard" {
		t.Fatalf("别名 dash 应解析到 dashboard，得到 %+v, %v", c, ok)
	}
	c, ok = MatchCommand("DETAIL") // 大小写不敏感
	if !ok || c.Name != "result" {
		t.Fatalf("别名 detail 应解析到 result，得到 %+v, %v", c, ok)
	}
	if _, ok = MatchCommand("command"); ok {
		t.Fatal("/command 不是有效命令，不应解析成功（占位符曾误导用户输入它）")
	}
}

func TestPrefixCommands(t *testing.T) {
	if got := len(PrefixCommands("")); got != len(CommandCatalog()) {
		t.Fatalf("空前缀应返回整个目录（%d），得到 %d", len(CommandCatalog()), got)
	}
	got := PrefixCommands("ca")
	if len(got) != 1 || got[0].Name != "cancel" {
		t.Fatalf("前缀 ca 应只命中 cancel，得到 %+v", got)
	}
	// 别名前缀也要命中（"/de" → detail 是 result 的别名）。
	got = PrefixCommands("de")
	if len(got) != 1 || got[0].Name != "result" {
		t.Fatalf("前缀 de 应经别名 detail 命中 result，得到 %+v", got)
	}
	if got := PrefixCommands("zzz"); len(got) != 0 {
		t.Fatalf("前缀 zzz 不应有命中，得到 %+v", got)
	}
}
