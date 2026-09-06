package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestModelRequestContractIsExplicitAndStreamFieldRetired(t *testing.T) {
	for _, input := range []string{"llm:\n  default_model: test\n", "llm:\n  request_contract: old\n", "llm:\n  request_contract: agentgo.model-request/v1\n  stream: true\n", "llm:\n  request_contract: agentgo.model-request/v1\n  stream: false\n"} {
		path := filepath.Join(t.TempDir(), "setting.yaml")
		if err := os.WriteFile(path, []byte(input), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path, true); err == nil {
			t.Fatalf("接受不兼容配置: %s", input)
		}
	}
	path := filepath.Join(t.TempDir(), "setting.yaml")
	if err := os.WriteFile(path, []byte("llm:\n  request_contract: agentgo.model-request/v1\n  default_model: test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path, true)
	if err != nil || !strings.HasSuffix(cfg.LLM.RequestContract, "/v1") {
		t.Fatalf("当前契约不能加载: %v", err)
	}
}
