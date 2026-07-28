package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Environment 基线的「模型 × Prompt × 配置」三元组（外加二进制 commit）。
// 任一变化都意味着换了参照系，旧基线不可比——对比前必须匹配。
type Environment struct {
	Model         string `json:"model"`
	PromptSHA256  string `json:"prompt_sha256"`  // 模板引用的 system_prompt_file 内容 hash
	ConfigSHA256  string `json:"config_sha256"`  // 抹除密钥后的模板配置 hash
	AgentGoCommit string `json:"agentgo_commit"` // 覆盖编译进二进制的 scheduler prompt 等
}

// BaselineTask 单个黄金任务的基线指标。
type BaselineTask struct {
	TerminalStatus   string  `json:"terminal_status"`
	JudgesPassed     bool    `json:"judges_passed"`
	WallSec          float64 `json:"wall_sec"`
	TotalTokens      int64   `json:"total_tokens"`
	LLMCalls         int     `json:"llm_calls"`
	Loops            int     `json:"loops"`
	Subtasks         int     `json:"subtasks"`
	Replans          int     `json:"replans"`
	AcceptanceRounds int     `json:"acceptance_rounds"`
	Errors           int     `json:"errors"`
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

// FingerprintEnvironment 从评测模板计算环境三元组。
// promptFiles 相对当前工作目录解析（与模板渲染后的子进程启动目录口径一致）。
func FingerprintEnvironment(templatePath string) (Environment, error) {
	data, err := os.ReadFile(templatePath)
	if err != nil {
		return Environment{}, fmt.Errorf("读取模板失败: %w", err)
	}

	// 模型与 prompt 文件清单用轻量结构提取（不做 ExpandEnv——三元组不碰密钥）
	var head struct {
		LLM struct {
			DefaultModel string `yaml:"default_model"`
		} `yaml:"llm"`
		Scheduler struct {
			Model string `yaml:"model"`
		} `yaml:"scheduler"`
		Agents []struct {
			SystemPromptFile string `yaml:"system_prompt_file"`
		} `yaml:"agents"`
	}
	if err := yaml.Unmarshal(data, &head); err != nil {
		return Environment{}, fmt.Errorf("模板 YAML 解析失败: %w", err)
	}
	env := Environment{Model: head.LLM.DefaultModel}
	if env.Model == "" {
		env.Model = head.Scheduler.Model
	}

	// prompt hash：按模板声明顺序串联各 prompt 文件内容
	ph := sha256.New()
	for _, a := range head.Agents {
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

	// 配置 hash：抹除全部密钥字段后对规范化 YAML 求 sha256——
	// 换密钥不算换参照系，基线不应因此失效
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return Environment{}, fmt.Errorf("模板 YAML 解析失败: %w", err)
	}
	if llm, ok := doc["llm"].(map[string]any); ok {
		delete(llm, "api_key")
	}
	delete(doc, "search_api_key")
	canonical, err := yaml.Marshal(doc)
	if err != nil {
		return Environment{}, fmt.Errorf("模板规范化失败: %w", err)
	}
	sum := sha256.Sum256(canonical)
	env.ConfigSHA256 = hex.EncodeToString(sum[:])

	env.AgentGoCommit = gitCommit()
	return env, nil
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
			TerminalStatus:   r.Metrics.TerminalStatus,
			JudgesPassed:     r.Passed,
			WallSec:          r.Metrics.WallSec,
			TotalTokens:      r.Metrics.PromptTokens + r.Metrics.CompletionTokens,
			LLMCalls:         r.Metrics.LLMCalls,
			Loops:            r.Metrics.Loops,
			Subtasks:         r.Metrics.Subtasks,
			Replans:          r.Metrics.Replans,
			AcceptanceRounds: r.Metrics.AcceptanceRounds,
			Errors:           r.Metrics.Errors,
		}
	}
	return b
}

// Compare 把本次运行结果与基线对比。band 是数值指标的宽容带（如 0.3 = ±30%）。
// 环境三元组不匹配时返回错误——调用方应跳过对比并提示重录基线。
func (b *Baseline) Compare(env Environment, results []TaskResult, band float64) ([]Alert, error) {
	if b.Environment != env {
		return nil, fmt.Errorf("环境三元组不匹配（模型/prompt/配置/commit 有变化）——这不是可对比的参照系，请先 agentgo eval record 重录基线")
	}
	var alerts []Alert
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
		if bt.JudgesPassed && !r.Passed {
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
		checkBand("replan 次数", float64(bt.Replans), float64(r.Metrics.Replans))
		checkBand("验收轮数", float64(bt.AcceptanceRounds), float64(r.Metrics.AcceptanceRounds))
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
