package bootstrap

// probe.go 实现 nextUpgrade_v4.md §9.5 启动期可观测性：
//   - printStartupBanner：在 probe 之前打印配置摘要 banner（§9.5.1）
//   - startupProbe：对 cfg.LLM.BaseURL 的 host:port 做 TCP DialTimeout（§9.5）
//
// 边界明示：probe 是 advisory 不是 authoritative——失败仅 warning（默认）；
// startup_probe="off" 跳过；startup_probe_failure_action="exit" 改为硬退出。
// best-effort 局限（mTLS / L4 LB 后端不健康 / CDN / WAF 等）见 §9.5.2。

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"time"

	"agentgo/internal/config"
)

// printStartupBanner 在配置校验通过后、运行 probe 之前打印配置摘要。
// 字段选择见 §9.5.1：列出所有 YAML 中显式配置过的内容；api_key 强制脱敏。
//
// configPath 为本次启动实际加载的配置文件路径——单独打印一行让用户
// 一眼确认走的是哪个 YAML，避免出现"以为在测 v4 但其实在跑默认 setting.yaml"
// 之类的误会（trace 显示 v3 命名规则但配置上 v4 字段都填了的迷之症状）。
//
// w 通常是 os.Stdout——传入参数便于单测拦截输出。
func printStartupBanner(w io.Writer, configPath string, cfg *config.Config) {
	fmt.Fprintln(w, "=== AgentGo Startup Configuration ===")
	fmt.Fprintf(w, "Config File:      %s\n", configPath)
	if cfg.LLM.BaseURL != "" {
		fmt.Fprintf(w, "LLM Endpoint:     %s\n", cfg.LLM.BaseURL)
	}
	if cfg.LLM.APIKey != "" {
		fmt.Fprintf(w, "LLM API Key:      %s\n", maskAPIKey(cfg.LLM.APIKey))
	}
	if cfg.LLM.DefaultModel != "" {
		fmt.Fprintf(w, "Default Model:    %s\n", cfg.LLM.DefaultModel)
	}
	if cfg.LLM.TimeoutSec > 0 {
		fmt.Fprintf(w, "Timeout:          %ds\n", cfg.LLM.TimeoutSec)
	}
	if len(cfg.Agents) > 0 {
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "Agent Kinds:")
		// scheduler 列在第一位（与 §9.5.1 示例输出一致）
		schedModel := cfg.Scheduler.Model
		if schedModel == "" {
			schedModel = cfg.LLM.DefaultModel
		}
		fmt.Fprintf(w, "  - scheduler   model=%s    (tools/prompt/behavior 全部 built-in，详见 §11.5.5)\n", schedModel)
		for _, k := range cfg.Agents {
			model := k.Model
			if model == "" {
				model = cfg.LLM.DefaultModel
			}
			profile := k.Profile
			if profile == "" && len(k.Tools) > 0 {
				profile = fmt.Sprintf("inline(%d tools)", len(k.Tools))
			}
			fmt.Fprintf(w, "  - %-10s × %d  model=%-20s profile=%-20s loops=%-3d ctx=%d\n",
				k.Kind, k.Replicas, model, profile, k.AgentMaxLoops, k.ContextLimit)
		}
	}
	fmt.Fprintln(w, "")
}

// maskAPIKey 把 API key 脱敏为 "前 4 + *** + 后 4 (length=N)" 形式。
// 长度 < 8 时整体替换为 *** 防止泄露。
func maskAPIKey(key string) string {
	n := len(key)
	if n < 8 {
		return "*** (length=" + intToStr(n) + ")"
	}
	return key[:4] + "***" + key[n-4:] + " (length=" + intToStr(n) + ")"
}

// intToStr 将 int 转为字符串。避免引入 strconv 增加 import。
func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// startupProbe 对 cfg.LLM.BaseURL 的 host:port 做 TCP DialTimeout。
//
// 行为分支：
//   - cfg.StartupProbe == "off"：直接返回 nil，跳过
//   - cfg.LLM.BaseURL 为空 / 解析失败：跳过（v3 兼容路径下 cfg.LLM 整块可能为空）
//   - DialTimeout 失败：返回 error。调用方根据 cfg.StartupProbeFailureAction
//     决定是 warning + 继续，还是硬退出
//
// 默认 timeout 5 秒（§9.5）；cfg.StartupProbeTimeoutSec > 0 时使用配置值。
func startupProbe(w io.Writer, cfg *config.Config) error {
	if cfg.StartupProbe == "off" {
		fmt.Fprintln(w, "=== Startup LLM Probe (disabled by startup_probe=off) ===")
		return nil
	}
	if cfg.LLM.BaseURL == "" {
		// v3 兼容路径：cfg.LLM 块未填，跳过——不当作错误
		return nil
	}
	target, err := extractHostPort(cfg.LLM.BaseURL)
	if err != nil {
		return fmt.Errorf("解析 llm.base_url=%q 失败: %w", cfg.LLM.BaseURL, err)
	}

	timeout := time.Duration(cfg.StartupProbeTimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	fmt.Fprintf(w, "=== Startup LLM Probe (level=tcp, timeout=%s) ===\n", timeout)
	start := time.Now()
	conn, dialErr := net.DialTimeout("tcp", target, timeout)
	elapsed := time.Since(start)
	if dialErr != nil {
		fmt.Fprintf(w, "  [FAIL] %s  (%v): %s\n", target, elapsed.Round(time.Millisecond), dialErr)
		fmt.Fprintln(w, "         This is a best-effort connectivity check. If you believe the endpoint is")
		fmt.Fprintln(w, "         reachable (e.g. mTLS / corporate gateway / CI mock), set startup_probe: off")
		fmt.Fprintln(w, "         in setting.yaml. Auth/model validity is always verified at first runtime call.")
		return dialErr
	}
	_ = conn.Close()
	fmt.Fprintf(w, "  [OK]   %s  (3-way handshake %v)\n", target, elapsed.Round(time.Millisecond))
	fmt.Fprintln(w, "  best-effort connectivity check; auth/model validity verified at first runtime call")
	return nil
}

// extractHostPort 把 https://dashscope.aliyuncs.com/compatible-mode/v1
// 解析为 "dashscope.aliyuncs.com:443"。
// 若 URL 缺端口，按 scheme 推断默认端口（http=80 / https=443）。
func extractHostPort(rawURL string) (string, error) {
	// url.Parse 接受 "host:port" 不带 scheme 的字符串但语义模糊；
	// 这里要求显式的 scheme://host[:port]/...
	if !strings.Contains(rawURL, "://") {
		// 容忍 "host:port" 简写
		host, port, err := net.SplitHostPort(rawURL)
		if err != nil {
			return "", fmt.Errorf("base_url 缺 scheme 且非 host:port: %s", rawURL)
		}
		return net.JoinHostPort(host, port), nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "http":
			port = "80"
		case "https":
			port = "443"
		default:
			return "", fmt.Errorf("未识别的 scheme %q（仅 http/https 推断默认端口）", u.Scheme)
		}
	}
	return net.JoinHostPort(host, port), nil
}
