// Package eval 实现行为评测体系（agentgo eval 子命令族）的驱动器。
//
// 评测驱动器对真实 agentgo 二进制做进程外黑盒驱动：经 dashboard 的
// /api/input 注入任务、收割 trace JSONL 聚合指标、跑确定性 judges。
// 当前落地的是所有 eval 子命令共用的第一阶段：凭证前置检查（preflight）——
// 环境变量未注入或 LLM 密钥被端点拒绝时立即失败退出，警告中给出
// HTTP 状态码、端点返回原文与排查提示，避免整个跑批在 401 中空转。
package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"agentgo/internal/config"
	"gopkg.in/yaml.v3"
)

// envVarPattern 匹配配置模板里的 ${VAR} 与 $VAR 环境变量引用，
// 与 os.ExpandEnv 的展开口径保持一致。
var envVarPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// probeBodyMaxLen 端点错误响应原文的保留上限：错误页可能是整份 HTML，
// 截断避免刷屏，截断标记一并写入报告。
const probeBodyMaxLen = 800

// defaultProbeTimeout 是模板未配置 llm.timeout_sec 时的探测超时。
const defaultProbeTimeout = 30 * time.Second

// ExtractEnvVars 提取模板中引用的全部环境变量名（去重、排序）。
// 解析 YAML 后只在标量值里找 ${VAR}/$VAR 引用——注释被解析器丢弃，
// 文档性文字（如用法说明里的 ${VAR} 示例）不会被误判为变量引用。
func ExtractEnvVars(template []byte) ([]string, error) {
	var root any
	if err := yaml.Unmarshal(template, &root); err != nil {
		return nil, err
	}
	names := map[string]bool{}
	walkEnvRefs(root, names)
	return sortedKeys(names), nil
}

// ExtractEnvVarsByScope 把变量按来源域分级：llm 块内引用是凭证硬依赖
// （缺失 = 401 全灭跑批，必须 fail-fast）；其余（如 search_api_key）
// 只影响附加能力，缺失降级为警告——黄金任务不依赖 web 搜索。
func ExtractEnvVarsByScope(template []byte) (llmVars, otherVars []string, err error) {
	var root map[string]any
	if err := yaml.Unmarshal(template, &root); err != nil {
		return nil, nil, err
	}
	llmSet := map[string]bool{}
	if llm, ok := root["llm"]; ok {
		walkEnvRefs(llm, llmSet)
	}
	allSet := map[string]bool{}
	walkEnvRefs(root, allSet)
	otherSet := map[string]bool{}
	for name := range allSet {
		if !llmSet[name] {
			otherSet[name] = true
		}
	}
	return sortedKeys(llmSet), sortedKeys(otherSet), nil
}

// walkEnvRefs 递归遍历 YAML 节点值，把字符串标量里的 ${VAR}/$VAR 引用收进 sink。
func walkEnvRefs(v any, sink map[string]bool) {
	switch t := v.(type) {
	case map[string]any:
		for _, val := range t {
			walkEnvRefs(val, sink)
		}
	case []any:
		for _, val := range t {
			walkEnvRefs(val, sink)
		}
	case string:
		for _, m := range envVarPattern.FindAllStringSubmatch(t, -1) {
			name := m[1]
			if name == "" {
				name = m[2]
			}
			sink[name] = true
		}
	}
}

// sortedKeys 返回集合的有序键列表。
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// MissingEnvVars 返回已引用但未注入（未设置或值为空串）的变量名。
// 空串按缺失处理：os.ExpandEnv 对未设置变量静默展开为空串，
// 空密钥会让端点 401 全灭整个跑批（config_v4_test.go 有记录）。
func MissingEnvVars(names []string, lookup func(string) (string, bool)) []string {
	var missing []string
	for _, n := range names {
		v, ok := lookup(n)
		if !ok || v == "" {
			missing = append(missing, n)
		}
	}
	return missing
}

// EnvMissingError 是「环境变量未注入」的前置检查失败。
type EnvMissingError struct {
	TemplatePath string
	Missing      []string
}

func (e *EnvMissingError) Error() string {
	return fmt.Sprintf(`凭证检查失败：环境变量未注入
  模板: %s
  缺失变量: %s
  提示: 请在当前终端注入后重试：
          export %s=<你的密钥>            # POSIX / Git Bash
          $env:%s="<你的密钥>"            # PowerShell
        注意：${VAR} 未设置时会被 os.ExpandEnv 静默展开为空串——
        空密钥会让端点 401 全灭整个跑批，本检查正是为此设防。`,
		e.TemplatePath, strings.Join(e.Missing, ", "),
		e.Missing[0], e.Missing[0])
}

// ProbeFailure 是一次失败的 LLM 探测：HTTP 层或传输层错误的结构化报告，
// Error() 输出含 HTTP 状态码、端点返回原文与排查提示的完整警告块。
type ProbeFailure struct {
	Endpoint   string // 完整探测 URL
	Model      string
	StatusCode int    // HTTP 状态码；传输层错误为 0
	Status     string // 如 "401 Unauthorized"；传输层错误为空
	Body       string // 截断后的端点返回原文；传输层错误为 Go 错误串
	Hint       string // 面向用户的排查提示
}

func (f *ProbeFailure) Error() string {
	status := f.Status
	if status == "" {
		status = "（无 HTTP 响应）"
	}
	return fmt.Sprintf(`LLM 密钥探测失败
  端点: POST %s
  模型: %s
  HTTP 状态码: %s
  端点返回: %s
  提示: %s`,
		f.Endpoint, f.Model, status, f.Body, f.Hint)
}

// probeLLM 向 OpenAI 兼容端点发一个 max_tokens=1 的最小 chat completion
// 验证密钥真实可用（/models 并非所有兼容端点都实现，且探测路径应与
// 评测实际调用路径一致）。返回 warning（非致命，如 429 限流）与
// failure（致命，含状态码/端点返回/提示）。
func probeLLM(ctx context.Context, client *http.Client, baseURL, apiKey, model string) (warning string, failure *ProbeFailure) {
	endpoint := strings.TrimRight(baseURL, "/") + "/chat/completions"
	reqBody := map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens": 1,
	}
	data, err := json.Marshal(reqBody)
	if err != nil {
		return "", &ProbeFailure{Endpoint: endpoint, Model: model, Body: err.Error(),
			Hint: "探测请求构造失败（内部错误），请把本输出原样反馈给维护者。"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return "", &ProbeFailure{Endpoint: endpoint, Model: model, Body: err.Error(),
			Hint: "base_url 不是合法 URL，请检查模板 llm.base_url（应为 https://主机/路径 形式，不要带末尾斜杠之外的路径缀）。"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := client.Do(req)
	if err != nil {
		return "", &ProbeFailure{Endpoint: endpoint, Model: model, Body: err.Error(),
			Hint: "无法连接端点。请检查：1. 当前网络可达该 base_url（代理/防火墙）；2. base_url 主机名拼写；3. 是否用了需要特殊网关的内网端点。"}
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, probeBodyMaxLen+1))
	body := string(raw)
	if len(raw) > probeBodyMaxLen {
		body = string(raw[:probeBodyMaxLen]) + "…（已截断）"
	}
	body = strings.TrimSpace(body)
	if body == "" {
		body = "（空响应体）"
	}

	fail := func(hint string) *ProbeFailure {
		return &ProbeFailure{Endpoint: endpoint, Model: model,
			StatusCode: resp.StatusCode, Status: resp.Status, Body: body, Hint: hint}
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return "", nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return "", fail("密钥被端点拒绝。请确认：\n" +
			"        1. 环境变量已在当前终端注入，且值无多余空格/引号；\n" +
			"        2. 密钥未过期、未撤销，且对该 base_url 有访问权限；\n" +
			"        3. base_url 与密钥所属服务商匹配（常见：把 A 家密钥配到 B 家端点）。")
	case resp.StatusCode == http.StatusNotFound:
		return "", fail("端点路径不存在。多半是 base_url 填错（缺 /v1 或路径多了一级）；\n" +
			"        少数端点对未知模型名也返回 404，可一并核对 default_model。")
	case resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Sprintf("端点返回 429 限流——密钥大概率有效，但跑批可能受挫，建议错峰或降低并发"), nil
	case resp.StatusCode == http.StatusBadRequest:
		return "", fail("请求被端点拒绝（参数或模型名问题）。请核对 default_model 在该端点真实存在；\n" +
			"        若模型名无误，把端点返回原文反馈给维护者以调整探测请求。")
	case resp.StatusCode >= 500:
		return "", fail("端点服务端错误，与密钥无关的可能性大。稍后重试 preflight；\n" +
			"        持续 5xx 请联系端点提供方。")
	default:
		return "", fail("未预期的状态码。请把本输出（状态码 + 端点返回）反馈给维护者。")
	}
}

// PreflightOptions 是一次凭证前置检查的输入。
type PreflightOptions struct {
	TemplatePath string
	HTTPClient   *http.Client                // nil = 按模板 llm.timeout_sec 构造
	LookupEnv    func(string) (string, bool) // nil = os.LookupEnv
}

// Preflight 执行凭证前置检查，顺序：模板存在性 → 环境变量注入 →
// 模板解析（经 config.LoadConfig，含 ${VAR} 展开）→ llm 字段非空 →
// 真实端点探测。任一环节失败立即返回可直接打印的完整警告块 error，
// 且环境变量缺失时绝不触碰网络。成功时向 stdout 打印检查报告。
func Preflight(ctx context.Context, opts PreflightOptions, stdout io.Writer) error {
	lookup := opts.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}

	data, err := os.ReadFile(opts.TemplatePath)
	if err != nil {
		return fmt.Errorf("凭证检查失败：无法读取评测配置模板\n  模板: %s\n  错误: %v\n  提示: 请参照 docs/design 的评测体系设计创建模板（llm 块用 ${VAR} 占位符引用密钥）。",
			opts.TemplatePath, err)
	}

	llmVars, otherVars, err := ExtractEnvVarsByScope(data)
	if err != nil {
		return fmt.Errorf("凭证检查失败：评测配置模板 YAML 解析失败\n  模板: %s\n  错误: %v\n  提示: 先用 agentgo config doctor -config %s 做静态检查排掉语法问题。",
			opts.TemplatePath, err, opts.TemplatePath)
	}
	// llm 块内引用是凭证硬依赖：缺失立即失败（不触碰网络）
	if missing := MissingEnvVars(llmVars, lookup); len(missing) > 0 {
		return &EnvMissingError{TemplatePath: opts.TemplatePath, Missing: missing}
	}
	// 其余引用（如 search_api_key）只降级附加能力，警告不致命
	softMissing := MissingEnvVars(otherVars, lookup)

	cfg, err := config.LoadConfig(opts.TemplatePath, true)
	if err != nil {
		return fmt.Errorf("凭证检查失败：评测配置模板解析失败\n  模板: %s\n  错误: %v\n  提示: 先用 agentgo config doctor -config %s 做静态检查排掉语法问题。",
			opts.TemplatePath, err, opts.TemplatePath)
	}

	// 模板整体过一遍 v4 校验：字段级错误（如 agents[*] 行为参数缺失）
	// 在跑批前 50ms 内暴露，而不是等子进程启动失败、健康等待空转 90 秒。
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("凭证检查失败：模板未通过 v4 配置校验\n  模板: %s\n  错误: %v\n  提示: 对照 config.example.yaml 或你 setting.yaml 的对应块补齐字段（常见：agents[*] 的 agent_max_loops/task_max_retries/enforce_compact_token_threshold/context_limit 四项必填且 >0）。",
			opts.TemplatePath, err)
	}

	if cfg.LLM.APIKey == "" {
		return fmt.Errorf("凭证检查失败：模板 llm.api_key 展开后为空\n  模板: %s\n  提示: 模板应写 ${VAR} 占位符且对应变量已注入；若确认变量已注入，检查占位符拼写是否与变量名一致（区分大小写）。",
			opts.TemplatePath)
	}
	if cfg.LLM.BaseURL == "" {
		return fmt.Errorf("凭证检查失败：模板 llm.base_url 为空\n  模板: %s\n  提示: 请对齐你 setting.yaml 的 llm.base_url。", opts.TemplatePath)
	}
	model := cfg.LLM.DefaultModel
	if model == "" {
		model = cfg.Scheduler.Model
	}
	if model == "" {
		return fmt.Errorf("凭证检查失败：模板未声明任何模型（llm.default_model 与 scheduler.model 均空）\n  模板: %s\n  提示: 请对齐你 setting.yaml 的 llm.default_model。", opts.TemplatePath)
	}

	client := opts.HTTPClient
	if client == nil {
		timeout := time.Duration(cfg.LLM.TimeoutSec) * time.Second
		if timeout <= 0 {
			timeout = defaultProbeTimeout
		}
		client = &http.Client{Timeout: timeout}
	}

	warning, failure := probeLLM(ctx, client, cfg.LLM.BaseURL, cfg.LLM.APIKey, model)
	if failure != nil {
		return failure
	}

	fmt.Fprintf(stdout, "[eval preflight] 凭证检查通过\n")
	fmt.Fprintf(stdout, "  模板: %s\n", opts.TemplatePath)
	if len(llmVars) > 0 {
		fmt.Fprintf(stdout, "  环境变量: %s ✓（已注入）\n", strings.Join(llmVars, ", "))
	} else {
		fmt.Fprintf(stdout, "  [提示] llm.api_key 未走环境变量占位符；若为明文，注意配置文件的扩散范围\n")
	}
	if len(softMissing) > 0 {
		fmt.Fprintf(stdout, "  [提示] 环境变量未注入（仅降级 web 搜索等附加能力，不影响评测）: %s\n", strings.Join(softMissing, ", "))
	}
	fmt.Fprintf(stdout, "  探测: POST %s/chat/completions → 200 OK\n", strings.TrimRight(cfg.LLM.BaseURL, "/"))
	fmt.Fprintf(stdout, "  模型: %s\n", model)
	if warning != "" {
		fmt.Fprintf(stdout, "  [警告] %s\n", warning)
	}
	return nil
}
