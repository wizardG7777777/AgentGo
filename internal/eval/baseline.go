package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"agentgo/internal/config"

	"gopkg.in/yaml.v3"
)

// Environment 是 V6 §7.7 的 Run Fingerprint：复现来源的全部关键事实。
// 可比性键（Model/PromptSHA256/ConfigSHA256/APIProtocol/EndpointHash/GraphSchema/SuiteDigest）
// 不一致时报 not_comparable；Commit/OS/Arch/BuildDirty 只是 provenance，
// 不因候选版本与基线 commit 不同就跳过回归比较。provider 概念已删除，不入指纹。
type Environment struct {
	Model         string `json:"model"`          // Scheduler 实际请求模型（含 scheduler.model 覆盖）
	PromptSHA256  string `json:"prompt_sha256"`  // 模板引用的 system_prompt_file 内容 hash
	ConfigSHA256  string `json:"config_sha256"`  // 环境展开、默认值合并并抹除密钥后的配置 hash
	AgentGoCommit string `json:"agentgo_commit"` // provenance：覆盖编译进二进制的 scheduler prompt 等

	APIProtocol  string `json:"api_protocol,omitempty"`  // 恒定 openai-compatible-chat-completions（V6 唯一协议）
	EndpointHash string `json:"endpoint_hash,omitempty"` // llm.base_url 的 sha256 前 12
	GraphSchema  string `json:"graph_schema,omitempty"`  // agentgo.graph/v1
	SuiteDigest  string `json:"suite_digest,omitempty"`  // 套件文件内容 sha256 前 12（由调用方填）
	OS           string `json:"os,omitempty"`            // provenance
	Arch         string `json:"arch,omitempty"`          // provenance
	BuildDirty   bool   `json:"build_dirty,omitempty"`   // provenance：构建时工作区是否有未提交改动
}

// apiProtocolV6 是 V6 唯一支持的 LLM 接口协议标识。
const apiProtocolV6 = "openai-compatible-chat-completions"

// graphSchemaV6 是 V6 Graph 执行契约的 schema 版本。
const graphSchemaV6 = "agentgo.graph/v1"

// BaselineTask 单个黄金任务的基线指标。
type BaselineTask struct {
	TerminalStatus string  `json:"terminal_status"`
	JudgesPassed   bool    `json:"judges_passed"`
	WallSec        float64 `json:"wall_sec"`
	TotalTokens    int64   `json:"total_tokens"`
	LLMCalls       int     `json:"llm_calls"`
	Loops          int     `json:"loops"`
	Errors         int     `json:"errors"`
}

// Baseline 一份基线文件（eval/baseline.json，本地文件不入库）。
type Baseline struct {
	Version     int                     `json:"version"`
	RecordedAt  string                  `json:"recorded_at"`
	Suite       string                  `json:"suite"`
	Environment Environment             `json:"environment"`
	Tasks       map[string]BaselineTask `json:"tasks"`
}

// Alert 对比告警：hard = 行为翻转（红）；soft = 数值超带（黄）。
type Alert struct {
	Level  string `json:"level"` // hard / soft
	Task   string `json:"task"`
	Detail string `json:"detail"`
}

// FingerprintEnvironment 从评测模板的实际展开配置计算运行指纹。
// system_prompt_file 相对当前工作目录解析（与子进程启动目录口径一致）。
func FingerprintEnvironment(templatePath string) (Environment, error) {
	cfg, err := config.LoadConfig(templatePath, true)
	if err != nil {
		return Environment{}, fmt.Errorf("加载展开后的评测配置失败: %w", err)
	}

	// Scheduler 实际优先使用 scheduler.model，未设置时才回落全局默认模型。
	env := Environment{Model: strings.TrimSpace(cfg.Scheduler.Model)}
	if env.Model == "" {
		env.Model = strings.TrimSpace(cfg.LLM.DefaultModel)
	}

	// prompt hash：按展开后的配置声明顺序串联各 prompt 文件内容。
	ph := sha256.New()
	for _, a := range cfg.Agents {
		if a.SystemPromptFile == "" {
			continue
		}
		content, err := os.ReadFile(a.SystemPromptFile)
		if err != nil {
			return Environment{}, fmt.Errorf("读取 prompt 文件 %s 失败: %w", a.SystemPromptFile, err)
		}
		ph.Write(content)
	}
	env.PromptSHA256 = hex.EncodeToString(ph.Sum(nil))

	// 配置 hash：基于运行时完成环境变量展开与默认值合并后的配置；密钥类字段
	// 统一清空，换密钥不改变参照系，也不会让密钥材料进入指纹输入。
	sanitized := *cfg
	sanitized.LLM.APIKey = ""
	sanitized.SearchAPIKey = ""
	sanitized.UI.Web.Token = ""
	canonical, err := yaml.Marshal(&sanitized)
	if err != nil {
		return Environment{}, fmt.Errorf("展开配置规范化失败: %w", err)
	}
	sum := sha256.Sum256(canonical)
	env.ConfigSHA256 = hex.EncodeToString(sum[:])

	// V6 Run Fingerprint 扩展：协议/端点/Graph schema 为可比性键；OS/Arch/Dirty 为 provenance
	env.APIProtocol = apiProtocolV6
	env.GraphSchema = graphSchemaV6
	baseURL := strings.TrimSpace(cfg.LLM.BaseURL)
	if baseURL != "" {
		es := sha256.Sum256([]byte(baseURL))
		env.EndpointHash = hex.EncodeToString(es[:])[:12]
	}
	env.OS = runtime.GOOS
	env.Arch = runtime.GOARCH
	env.BuildDirty = gitDirty()

	env.AgentGoCommit = gitCommit()
	return env, nil
}

// gitDirty 报告工作区是否有未提交改动；非 git 环境降级 false（无从判定，不阻断）。
func gitDirty() bool {
	out, err := exec.Command("git", "status", "--porcelain").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != ""
}

// gitCommit 取当前仓库 HEAD；失败（非 git 环境）降级为 "unknown"。
func gitCommit() string {
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

// LoadBaseline 读取基线文件；不存在返回 (nil, nil)——首次运行无基线是正常路径。
func LoadBaseline(path string) (*Baseline, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var b Baseline
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("基线文件解析失败: %w", err)
	}
	return &b, nil
}

// SaveBaseline 写基线文件（父目录自动创建）。
func SaveBaseline(path string, b *Baseline) error {
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return err
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// NewBaseline 从一次运行结果生成基线。
func NewBaseline(suiteName string, env Environment, results []TaskResult) *Baseline {
	b := &Baseline{
		Version:     1,
		RecordedAt:  time.Now().Format(time.RFC3339),
		Suite:       suiteName,
		Environment: env,
		Tasks:       map[string]BaselineTask{},
	}
	for _, r := range results {
		b.Tasks[r.Name] = BaselineTask{
			TerminalStatus: r.Metrics.TerminalStatus,
			JudgesPassed:   r.Status == StatusPass,
			WallSec:        r.Metrics.WallSec,
			TotalTokens:    r.Metrics.PromptTokens + r.Metrics.CompletionTokens,
			LLMCalls:       r.Metrics.LLMCalls,
			Loops:          r.Metrics.Loops,
			Errors:         r.Metrics.Errors,
		}
	}
	return b
}

// comparabilityDiffs 列出可比性键的差异名（空 = 可比）。
// 键的选择遵循 V6 §7.7：行为正确性要求模型/prompt/配置/调用协议一致；
// 套件内容不同则基线行根本不对应。
func (e Environment) comparabilityDiffs(o Environment) []string {
	var diffs []string
	if e.Model != o.Model {
		diffs = append(diffs, "model")
	}
	if e.PromptSHA256 != o.PromptSHA256 {
		diffs = append(diffs, "prompt_digest")
	}
	if e.ConfigSHA256 != o.ConfigSHA256 {
		diffs = append(diffs, "config_digest")
	}
	if e.APIProtocol != o.APIProtocol {
		diffs = append(diffs, "api_protocol")
	}
	if e.EndpointHash != o.EndpointHash {
		diffs = append(diffs, "endpoint_hash")
	}
	if e.GraphSchema != o.GraphSchema {
		diffs = append(diffs, "graph_schema")
	}
	if e.SuiteDigest != o.SuiteDigest {
		diffs = append(diffs, "suite_digest")
	}
	return diffs
}

// provenanceDiffs 列出 provenance 差异名（仅记录，不影响可比性）。
func (e Environment) provenanceDiffs(o Environment) []string {
	var notes []string
	if e.AgentGoCommit != o.AgentGoCommit {
		notes = append(notes, "agentgo_commit")
	}
	if e.OS != o.OS || e.Arch != o.Arch {
		notes = append(notes, "os/arch")
	}
	if e.BuildDirty != o.BuildDirty {
		notes = append(notes, "build_dirty")
	}
	return notes
}

// Compare 把本次运行结果与基线对比。band 是数值指标的宽容带（如 0.3 = ±30%）。
// 可比性键（模型/prompt/配置/协议/端点/graph schema/套件）不一致时返回单条
// not_comparable 告警并列出差异项——不强行混组，也不再整体拒绝；
// commit/OS/Arch/Dirty 差异只作 provenance 记录（info 级，不阻断比较）。
func (b *Baseline) Compare(env Environment, results []TaskResult, band float64) ([]Alert, error) {
	var alerts []Alert
	if diffs := b.Environment.comparabilityDiffs(env); len(diffs) > 0 {
		alerts = append(alerts, Alert{Level: "not_comparable", Task: "*",
			Detail: "指纹关键项不同，失去可比性: " + strings.Join(diffs, ", ") + "——请显式重录基线（record + promote）"})
		return alerts, nil
	}
	if notes := b.Environment.provenanceDiffs(env); len(notes) > 0 {
		alerts = append(alerts, Alert{Level: "info", Task: "*",
			Detail: "provenance 差异（不影响可比性）: " + strings.Join(notes, ", ")})
	}
	for _, r := range results {
		bt, ok := b.Tasks[r.Name]
		if !ok {
			alerts = append(alerts, Alert{Level: "soft", Task: r.Name, Detail: "基线中无此任务记录（新任务？）"})
			continue
		}
		if r.Metrics.TerminalStatus != bt.TerminalStatus {
			alerts = append(alerts, Alert{Level: "hard", Task: r.Name,
				Detail: fmt.Sprintf("终态翻转: 基线 %s → 本次 %s", bt.TerminalStatus, r.Metrics.TerminalStatus)})
		}
		if bt.JudgesPassed && r.Status != StatusPass {
			alerts = append(alerts, Alert{Level: "hard", Task: r.Name,
				Detail: "判据由全通过转为存在失败"})
		}
		checkBand := func(label string, base, got float64) {
			if base <= 0 {
				return // 基线为 0 的指标不做比例对比
			}
			delta := (got - base) / base
			if delta > band || delta < -band {
				alerts = append(alerts, Alert{Level: "soft", Task: r.Name,
					Detail: fmt.Sprintf("%s 超带: 基线 %.0f → 本次 %.0f（%+.0f%%，带宽 ±%.0f%%）", label, base, got, delta*100, band*100)})
			}
		}
		checkBand("总 token", float64(bt.TotalTokens), float64(r.Metrics.PromptTokens+r.Metrics.CompletionTokens))
		checkBand("耗时", bt.WallSec, r.Metrics.WallSec)
		checkBand("loops", float64(bt.Loops), float64(r.Metrics.Loops))
		checkBand("错误数", float64(bt.Errors), float64(r.Metrics.Errors))
	}
	// 基线有而本次没跑的任务
	run := map[string]bool{}
	for _, r := range results {
		run[r.Name] = true
	}
	for name := range b.Tasks {
		if !run[name] {
			alerts = append(alerts, Alert{Level: "soft", Task: name, Detail: "本次未运行（基线中有记录）"})
		}
	}
	return alerts, nil
}
