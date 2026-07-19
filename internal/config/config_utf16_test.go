package config

import (
	"os"
	"path/filepath"
	"testing"
	"unicode/utf16"
)

// encodeUTF16 把字符串编码为 UTF-16（带 BOM），模拟 Windows 记事本保存格式。
func encodeUTF16(s string, bigEndian bool) []byte {
	u16 := utf16.Encode([]rune(s))
	out := make([]byte, 0, len(u16)*2+2)
	if bigEndian {
		out = append(out, 0xFE, 0xFF)
	} else {
		out = append(out, 0xFF, 0xFE)
	}
	for _, u := range u16 {
		if bigEndian {
			out = append(out, byte(u>>8), byte(u))
		} else {
			out = append(out, byte(u), byte(u>>8))
		}
	}
	return out
}

// TestLoadConfig_UTF16LE_ExpandsEnv 验证 E1 修复：UTF-16LE 配置文件中的
// ${VAR} 能被正常展开（此前 ExpandEnv 在交错 NUL 字节上失效，静默保留字面量）。
func TestLoadConfig_UTF16LE_ExpandsEnv(t *testing.T) {
	t.Setenv("E1_TEST_KEY", "sk-expanded-secret")

	content := "llm:\n  api_key: ${E1_TEST_KEY}\n  default_model: test-model\n"
	path := filepath.Join(t.TempDir(), "setting.yaml")
	if err := os.WriteFile(path, encodeUTF16(content, false), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path, true)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.LLM.APIKey != "sk-expanded-secret" {
		t.Fatalf("APIKey = %q, want %q（UTF-16 下环境变量未展开）", cfg.LLM.APIKey, "sk-expanded-secret")
	}
	if cfg.LLM.DefaultModel != "test-model" {
		t.Fatalf("DefaultModel = %q, want %q", cfg.LLM.DefaultModel, "test-model")
	}
}

// TestLoadConfig_UTF16BE_ExpandsEnv 验证 BE 变体同样被转码。
func TestLoadConfig_UTF16BE_ExpandsEnv(t *testing.T) {
	t.Setenv("E1_TEST_KEY_BE", "be-secret")

	content := "llm:\n  api_key: ${E1_TEST_KEY_BE}\n"
	path := filepath.Join(t.TempDir(), "setting.yaml")
	if err := os.WriteFile(path, encodeUTF16(content, true), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path, true)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.LLM.APIKey != "be-secret" {
		t.Fatalf("APIKey = %q, want %q", cfg.LLM.APIKey, "be-secret")
	}
}

// TestDecodeIfUTF16_Passthrough 无 BOM 输入原样返回；过短输入安全。
func TestDecodeIfUTF16_Passthrough(t *testing.T) {
	plain := []byte("llm:\n  api_key: plain\n")
	if got := decodeIfUTF16(plain); string(got) != string(plain) {
		t.Fatalf("UTF-8 输入被改写: %q", got)
	}
	if got := decodeIfUTF16([]byte{0xFF}); len(got) != 1 {
		t.Fatalf("单字节输入应原样返回, got %v", got)
	}
	if got := decodeIfUTF16(nil); got != nil {
		t.Fatalf("nil 输入应原样返回, got %v", got)
	}
}
