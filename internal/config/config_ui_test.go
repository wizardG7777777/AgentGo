package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validUIBase 返回一份最小合法 Config（Scheduler-only 模式），测试只改 ui 块。
func validUIBase() *Config {
	return &Config{
		LLM:         LLMConfig{DefaultModel: "gpt-test"},
		ProjectRoot: ".",
		UI: UIConfig{
			Frontends: []string{"tui"},
			Web:       WebUIConfig{Listen: "127.0.0.1:8399"},
		},
	}
}

// TestUIConfig_DefaultsApplied 未声明 ui 块时回落默认值：frontends=[tui]、
// listen=127.0.0.1:8399、token 为空；声明了部分字段时未提及字段保持默认。
func TestUIConfig_DefaultsApplied(t *testing.T) {
	dir := t.TempDir()

	// 完全不写 ui 块
	p1 := filepath.Join(dir, "a.yaml")
	if err := os.WriteFile(p1, []byte("llm:\n  default_model: gpt-test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(p1, true)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.UI.Frontends) != 1 || cfg.UI.Frontends[0] != "tui" {
		t.Fatalf("默认 frontends = %v，期望 [tui]", cfg.UI.Frontends)
	}
	if cfg.UI.Web.Listen != "127.0.0.1:8399" {
		t.Fatalf("默认 listen = %q，期望 127.0.0.1:8399", cfg.UI.Web.Listen)
	}
	if cfg.UI.Web.Token != "" {
		t.Fatalf("默认 token = %q，期望空", cfg.UI.Web.Token)
	}

	// 只写 frontends，web.listen 保持默认
	p2 := filepath.Join(dir, "b.yaml")
	if err := os.WriteFile(p2, []byte("llm:\n  default_model: gpt-test\nui:\n  frontends: [tui, web]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg2, err := LoadConfig(p2, true)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg2.UI.HasFrontend("web") || cfg2.UI.Web.Listen != "127.0.0.1:8399" {
		t.Fatalf("部分覆盖后 ui = %+v", cfg2.UI)
	}
	if err := cfg2.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// TestValidate_UIRejectsInvalidFrontend 非法 frontend 取值被拒绝。
func TestValidate_UIRejectsInvalidFrontend(t *testing.T) {
	cfg := validUIBase()
	cfg.UI.Frontends = []string{"tui", "gui"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "gui") {
		t.Fatalf("应拒绝非法 frontend，err = %v", err)
	}
}

// TestValidate_UIDedupesFrontends 重复 frontend 被去重且保序。
func TestValidate_UIDedupesFrontends(t *testing.T) {
	cfg := validUIBase()
	cfg.UI.Frontends = []string{"web", "tui", "web"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(cfg.UI.Frontends) != 2 || cfg.UI.Frontends[0] != "web" || cfg.UI.Frontends[1] != "tui" {
		t.Fatalf("去重后 frontends = %v", cfg.UI.Frontends)
	}
}

// TestValidate_UINonLoopbackRequiresToken 非 loopback 监听无 token 拒绝；
// 有 token 接受；loopback（127.0.0.1 / ::1 / localhost）无 token 接受。
func TestValidate_UINonLoopbackRequiresToken(t *testing.T) {
	cases := []struct {
		name    string
		listen  string
		token   string
		wantErr bool
	}{
		{"loopback-v4 无 token", "127.0.0.1:8399", "", false},
		{"loopback-v6 无 token", "[::1]:8399", "", false},
		{"localhost 无 token", "localhost:8399", "", false},
		{"全网卡 v4 无 token", "0.0.0.0:8399", "", true},
		{"全网卡 v6 无 token", "[::]:8399", "", true},
		{"空 host 无 token", ":8399", "", true},
		{"局域网 IP 无 token", "192.168.1.10:8399", "", true},
		{"公网 IP 无 token", "8.8.8.8:8399", "", true},
		{"非 loopback 有 token", "0.0.0.0:8399", "secret-token", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validUIBase()
			cfg.UI.Frontends = []string{"web"}
			cfg.UI.Web.Listen = tc.listen
			cfg.UI.Web.Token = tc.token
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("listen=%q token=%q 应被拒绝", tc.listen, tc.token)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("listen=%q token=%q 应被接受，err = %v", tc.listen, tc.token, err)
			}
			if tc.wantErr && err != nil && !strings.Contains(err.Error(), "token") {
				t.Fatalf("错误信息应解释需设置 token，err = %v", err)
			}
		})
	}
}

// TestValidate_UIRejectsMalformedListen 无法解析为 host:port 的 listen 被拒绝。
func TestValidate_UIRejectsMalformedListen(t *testing.T) {
	for _, listen := range []string{"not-an-addr", "127.0.0.1", "127.0.0.1:abc", "127.0.0.1:99999"} {
		cfg := validUIBase()
		cfg.UI.Frontends = []string{"web"}
		cfg.UI.Web.Listen = listen
		if err := cfg.Validate(); err == nil {
			t.Fatalf("listen=%q 应被拒绝", listen)
		}
	}
}

// TestValidate_UIWebChecksSkippedWithoutWebFrontend 未启用 web 前端时不校验 listen
// （默认配置 frontends=[tui] 不应因 web 块而失败）。
func TestValidate_UIWebChecksSkippedWithoutWebFrontend(t *testing.T) {
	cfg := validUIBase()
	cfg.UI.Frontends = []string{"tui"}
	cfg.UI.Web.Listen = "0.0.0.0:8399" // 未启用 web，忽略
	if err := cfg.Validate(); err != nil {
		t.Fatalf("未启用 web 时不应校验 listen，err = %v", err)
	}
}
