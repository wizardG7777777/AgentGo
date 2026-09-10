package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRetiredSubtaskDepthRejectsExplicitZeroAndPositiveValues(t *testing.T) {
	for _, value := range []string{"0", "3", "null"} {
		t.Run(value, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			body := "graph:\n  request_contract: agentgo.graph/v6\nllm:\n  request_contract: agentgo.model-request/v1\nmax_subtask_depth: " + value + "\n"
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadConfig(path, true)
			if err == nil || !strings.Contains(err.Error(), "max_subtask_depth 已退役") {
				t.Fatalf("旧字段未明确拒绝：%v", err)
			}
		})
	}
}
