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
	cfgPath := filepath.Join(tmpDir, "setting.yaml")
	cfg := []byte("worker_count: 1\nscheduler_ticker_sec: 100\n")
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

func TestMain_DefaultConfigPathMissing_ShouldFallbackAndExitOnEOF(t *testing.T) {
	tmpDir := t.TempDir()
	// 不传 -config，使用默认 setting.yaml；在 tmpDir 下该文件不存在
	result := runMainAsSubprocess(t, tmpDir)

	if result.exitCode != 0 {
		t.Fatalf("expected zero exit code with default fallback config, got %d; stdout=%s stderr=%s", result.exitCode, result.stdout, result.stderr)
	}
	if !strings.Contains(result.stderr, "默认配置文件") {
		t.Fatalf("stderr should contain default-config warning, got: %s", result.stderr)
	}
	if !strings.Contains(result.stdout, "[启动] 系统就绪，等待用户输入") {
		t.Fatalf("stdout should contain system ready message, got: %s", result.stdout)
	}
}
