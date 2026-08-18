package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWebUIConfig_AutoOpenDefault 验证 auto_open 未设置时默认关闭（nil 三态）。
func TestWebUIConfig_AutoOpenDefault(t *testing.T) {
	var c WebUIConfig
	if c.AutoOpenEnabled() {
		t.Fatal("auto_open 未设置时应默认关闭")
	}
}

// TestWebUIConfig_AutoOpenExplicitFalse 验证显式 false 关闭自动打开。
func TestWebUIConfig_AutoOpenExplicitFalse(t *testing.T) {
	off := false
	c := WebUIConfig{AutoOpen: &off}
	if c.AutoOpenEnabled() {
		t.Fatal("auto_open: false 应关闭自动打开")
	}
	on := true
	c2 := WebUIConfig{AutoOpen: &on}
	if !c2.AutoOpenEnabled() {
		t.Fatal("auto_open: true 应保持开启")
	}
}

// TestLoadConfig_AutoOpenParsed 验证 YAML 中 auto_open: false 被正确解析。
func TestLoadConfig_AutoOpenParsed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "setting.yaml")
	content := "ui:\n  frontends: [web]\n  web:\n    listen: \"127.0.0.1:8399\"\n    auto_open: false\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path, true)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.UI.Web.AutoOpenEnabled() {
		t.Fatal("YAML auto_open: false 未生效")
	}
}
