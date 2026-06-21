package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	mainHelperEnv     = "AGENTGO_MAIN_HELPER"
	mainHelperArgsEnv = "AGENTGO_MAIN_ARGS_JSON"
)

// TestMainHelperProcess 作为子进程入口，专门用于调用 main()。
// 注意：此函数不应在普通测试流程中执行。
func TestMainHelperProcess(t *testing.T) {
	if os.Getenv(mainHelperEnv) != "1" {
		return
	}

	var args []string
	if raw := os.Getenv(mainHelperArgsEnv); raw != "" {
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			t.Fatalf("failed to parse %s: %v", mainHelperArgsEnv, err)
		}
	}

	// 重置 main 程序看到的参数列表（不包含 go test 参数）
	os.Args = append([]string{"agentgo"}, args...)
	main()
}

type mainRunResult struct {
	exitCode int
	stdout   string
	stderr   string
}

func runMainAsSubprocess(t *testing.T, dir string, args ...string) mainRunResult {
	t.Helper()

	argJSON, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestMainHelperProcess")
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(),
		mainHelperEnv+"=1",
		mainHelperArgsEnv+"="+string(argJSON),
	)
	// 让 CLI 立即收到 EOF，触发正常退出路径
	cmd.Stdin = bytes.NewBuffer(nil)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	result := mainRunResult{
		exitCode: 0,
		stdout:   stdout.String(),
		stderr:   stderr.String(),
	}

	if runErr == nil {
		return result
	}

	if exitErr, ok := runErr.(*exec.ExitError); ok {
		result.exitCode = exitErr.ExitCode()
		return result
	}

	t.Fatalf("unexpected run error: %v", runErr)
	return result
}

func TestMain_ExplicitConfigMissing_ShouldExitNonZero(t *testing.T) {
	tmpDir := t.TempDir()
	missingPath := filepath.Join(tmpDir, "not-exist.yaml")

	result := runMainAsSubprocess(t, tmpDir, "-config", missingPath)
	if result.exitCode == 0 {
		t.Fatalf("expected non-zero exit code when explicit config is missing; stdout=%s stderr=%s", result.stdout, result.stderr)
	}
	if !strings.Contains(result.stderr, "启动失败") {
		t.Fatalf("stderr should contain startup failure message, got: %s", result.stderr)
	}
	if !strings.Contains(result.stderr, "配置文件不存在") {
		t.Fatalf("stderr should mention missing config, got: %s", result.stderr)
	}
}

func TestMain_ExplicitConfigValid_ShouldStartAndExitOnEOF(t *testing.T) {
	tmpDir := t.TempDir()
	promptPath := filepath.Join(tmpDir, "worker.md")
	if err := os.WriteFile(promptPath, []byte("test worker prompt\n"), 0o644); err != nil {
		t.Fatalf("write prompt failed: %v", err)
	}
	// v4 必填：agents 列表 + 每个 kind 的所有行为参数 + system_prompt_file 可读
	cfgPath := filepath.Join(tmpDir, "setting.yaml")
	cfg := []byte(`agents:
  - kind: worker
    replicas: 1
    tools: [read_file]
    system_prompt_file: ` + filepath.ToSlash(promptPath) + `
    agent_max_loops: 5
    task_max_retries: 1
    enforce_compact_token_threshold: 4000
    context_limit: 16000
startup_probe: "off"
`)
	if err := os.WriteFile(cfgPath, cfg, 0o644); err != nil {
		t.Fatalf("write config failed: %v", err)
	}

	result := runMainAsSubprocess(t, tmpDir, "-config", cfgPath)
	if result.exitCode != 0 {
		t.Fatalf("expected zero exit code, got %d; stdout=%s stderr=%s", result.exitCode, result.stdout, result.stderr)
	}
	if !strings.Contains(result.stdout, "[启动] 系统就绪，等待用户输入") {
		t.Fatalf("stdout should contain system ready message, got: %s", result.stdout)
	}
	if !strings.Contains(result.stdout, "[关闭] 系统已停止") {
		t.Fatalf("stdout should contain shutdown message, got: %s", result.stdout)
	}
}

func TestMain_DefaultConfigPathMissing_ShouldFailFast(t *testing.T) {
	// v3 兼容层 2026-04-26 删除后，无 setting.yaml + 内置默认配置不再能启动——
	// 因为 v4 §11.5.3 校验要求 agents 列表非空。本测试断言这一 fail-fast 行为，
	// 取代旧的"fallback to defaults" 期望（历史问题记录中的"启动成功 ≠ 真正可用"
	// 反模式由此被 v4 修正）。
	tmpDir := t.TempDir()
	result := runMainAsSubprocess(t, tmpDir)

	if result.exitCode == 0 {
		t.Fatalf("expected non-zero exit code when default config is missing in v4, got 0; stdout=%s stderr=%s", result.stdout, result.stderr)
	}
	if !strings.Contains(result.stderr, "默认配置文件") {
		t.Fatalf("stderr should contain default-config warning, got: %s", result.stderr)
	}
	if !strings.Contains(result.stderr, "agents 列表为空") {
		t.Fatalf("stderr should explain v4 validation failure, got: %s", result.stderr)
	}
}
