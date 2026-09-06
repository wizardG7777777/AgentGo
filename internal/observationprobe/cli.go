package observationprobe

import (
	"agentgo/internal/contextruntime"
	"agentgo/internal/contextstore"
	"agentgo/internal/policycatalog"
	"agentgo/internal/session"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"agentgo/internal/config"
	"agentgo/internal/llm"
	"agentgo/internal/observationcontract"
)

type attemptFailure struct {
	Kind         string `json:"failure_kind"`
	ProviderCode string `json:"provider_code,omitempty"`
}

type report struct {
	Model       string           `json:"model"`
	Profile     string           `json:"profile"`
	Fixture     string           `json:"fixture"`
	Attempts    int              `json:"attempts"`
	Successes   int              `json:"successes"`
	SuccessRate float64          `json:"success_rate"`
	Failures    []attemptFailure `json:"failures,omitempty"`
}

// CLI runs only provider capability checks. It never starts Bootstrap or writes
// project runtime state, and its JSON deliberately excludes keys/prompts/reasoning.
func CLI(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "observation" {
		fmt.Fprintln(stderr, "用法: agentgo probe observation -config <path> --model <name> --profile <v7|v8|v9|v10|v11|v12> --fixture <empty|populated> --attempts 3 --json")
		return 2
	}
	fs := flag.NewFlagSet("probe observation", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "setting.yaml", "配置文件")
	model := fs.String("model", "", "待探测模型")
	profile := fs.String("profile", "v12", "v7、v8、v9、v10、v11 或 v12")
	fixture := fs.String("fixture", "empty", "empty 或 populated")
	attempts := fs.Int("attempts", 3, "尝试次数")
	configured := fs.Bool("configured", false, "探测配置中实际 Observation 模型")
	jsonOutput := fs.Bool("json", false, "输出脱敏 JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if (*profile != "v7" && *profile != "v8" && *profile != "v9" && *profile != "v10" && *profile != "v11" && *profile != "v12") || (*fixture != "empty" && *fixture != "populated") || *attempts <= 0 || *attempts > 10 {
		fmt.Fprintln(stderr, "profile/fixture/attempts 参数非法")
		return 2
	}
	cfg, err := config.LoadConfig(*configPath, true)
	if err != nil {
		fmt.Fprintf(stderr, "配置加载失败: %v\n", err)
		return 1
	}
	models := []string{strings.TrimSpace(*model)}
	if *configured {
		models = configuredModels(cfg)
	}
	if len(models) == 0 || models[0] == "" {
		fmt.Fprintln(stderr, "必须指定 --model 或 --configured")
		return 2
	}
	reports := make([]report, 0, len(models))
	exitCode := 0
	for _, current := range models {
		rep := run(cfg, current, *profile, *fixture, *attempts)
		reports = append(reports, rep)
		if rep.Successes != rep.Attempts {
			exitCode = 4
		}
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	if *jsonOutput || len(reports) > 1 {
		_ = encoder.Encode(reports)
	} else {
		_ = encoder.Encode(reports[0])
	}
	return exitCode
}

func configuredModels(cfg *config.Config) []string {
	seen := make(map[string]struct{})
	var models []string
	for _, kind := range cfg.Agents {
		// Configured preflight follows the default Graph agent route. Read-only
		// explorer and acceptance.verify kinds do not receive code-change Progress.
		if strings.TrimSpace(kind.EventType) != "" {
			continue
		}
		model := strings.TrimSpace(kind.ObservationModel)
		if model == "" {
			model = strings.TrimSpace(kind.Model)
		}
		if model == "" {
			model = strings.TrimSpace(cfg.LLM.DefaultModel)
		}
		if model == "" {
			continue
		}
		if _, ok := seen[model]; ok {
			continue
		}
		seen[model] = struct{}{}
		models = append(models, model)
	}
	sort.Strings(models)
	return models
}

func run(cfg *config.Config, modelName, profile, fixture string, attempts int) report {
	profileData := observationcontract.SchemaProfile{}
	if fixture == "populated" {
		profileData = observationcontract.SchemaProfile{
			EvidenceRefs:            []string{"tool-call:probe-read"},
			OpenCandidateRefs:       []string{"candidate:sha256:probe"},
			PostPredecessorEvidence: []string{"tool-call:probe-check"},
		}
	}
	parameters := observationcontract.Parameters(profileData)
	tool := llm.ToolDef{Name: "record_observation_delta", Description: "提交探针 fixture", Parameters: parameters}
	choice := llm.ToolChoice{Mode: llm.ToolChoiceFunction, Name: tool.Name}
	reasoning := "none"
	if profile == "v8" || profile == "v9" || profile == "v10" || profile == "v11" || profile == "v12" {
		choice = llm.ToolChoice{Mode: llm.ToolChoiceAuto}
		reasoning = "low"
	}
	client := llm.NewTransport(llm.TransportConfig{BaseURL: cfg.LLM.BaseURL, APIKey: cfg.LLM.APIKey, Timeout: timeout(cfg.LLM.TimeoutSec)})
	rep := report{Model: modelName, Profile: profile, Fixture: fixture, Attempts: attempts}
	snapshots, err := contextstore.New(filepath.Join(cfg.ProjectRoot, ".agentgo", "state", "observation-probes-v2"))
	if err != nil {
		rep.Failures = append(rep.Failures, attemptFailure{Kind: "context_store_failure"})
		return rep
	}
	defer snapshots.Close()
	policies, err := policycatalog.NewDefault()
	if err != nil {
		rep.Failures = append(rep.Failures, attemptFailure{Kind: "context_policy_failure"})
		return rep
	}
	runtime := contextruntime.Runtime{Assembler: contextruntime.NewAssembler(), Policies: policies, Snapshots: snapshots}
	runtime.Output = contextruntime.NewOutputService(func(record contextruntime.OutputRecord) error {
		return session.AppendModelOutputFile(filepath.Join(cfg.ProjectRoot, ".agentgo", "state", "observation-probes-v2", "model-outputs.jsonl"), record)
	})
	options := cfg.LLM.InvocationOptions(modelName)
	options.ToolChoice = choice
	options.ReasoningEffort = reasoning
	options.OutputBudget = probeBudget(profile)
	for index := 0; index < attempts; index++ {
		response, err := runtime.InvokeOperation(context.Background(), contextruntime.Instructions{ProfileID: "observation:" + profile, Objective: fixturePrompt(fixture)}, nil, []llm.ToolDef{tool}, options, client)
		if err == nil {
			err = validateResponse(response, fixture)
		}
		if err == nil {
			rep.Successes++
			continue
		}
		failure := attemptFailure{Kind: "unknown"}
		if typed, ok := llm.FromError(err); ok {
			failure.Kind = string(typed.Kind)
			failure.ProviderCode = typed.ProviderCode
		}
		rep.Failures = append(rep.Failures, failure)
	}
	rep.SuccessRate = float64(rep.Successes) / float64(rep.Attempts)
	return rep
}

func fixturePrompt(fixture string) string {
	if fixture == "empty" {
		return `Call record_observation_delta exactly once with phase="investigate", facts=[], resolved_candidates=[], next_candidates=[], and next_action={"decision":"continue"}. Do not answer with text.`
	}
	return `Call record_observation_delta exactly once with phase="investigate", one fact citing tool-call:probe-read, one resolved candidate candidate:sha256:probe citing tool-call:probe-check, next_candidates=[], and next_action={"decision":"continue"}. Do not answer with text.`
}

func validateResponse(response llm.Result, fixture string) error {
	if len(response.ToolCalls()) == 0 || response.ToolCalls()[0].Name != "record_observation_delta" {
		return fmt.Errorf("缺少必需 Observation tool call")
	}
	args := response.ToolCalls()[0].Arguments
	if args["phase"] != "investigate" {
		return fmt.Errorf("phase 非法")
	}
	facts, factsOK := args["facts"].([]any)
	resolved, resolvedOK := args["resolved_candidates"].([]any)
	if !factsOK || !resolvedOK {
		return fmt.Errorf("facts/resolved_candidates 必须是数组")
	}
	if fixture == "empty" && (len(facts) != 0 || len(resolved) != 0) {
		return fmt.Errorf("empty fixture 未提交空 authority")
	}
	if fixture == "populated" && (len(facts) != 1 || len(resolved) != 1) {
		return fmt.Errorf("populated fixture 未使用 authority")
	}
	if fixture == "populated" {
		fact, factOK := facts[0].(map[string]any)
		resolution, resolutionOK := resolved[0].(map[string]any)
		if !factOK || !resolutionOK || resolution["candidate_ref"] != "candidate:sha256:probe" ||
			!containsString(fact["evidence_refs"], "tool-call:probe-read") ||
			!containsString(resolution["evidence_refs"], "tool-call:probe-check") {
			return fmt.Errorf("populated fixture authority 参数不匹配")
		}
	}
	nextAction, ok := args["next_action"].(map[string]any)
	if !ok || nextAction["decision"] != "continue" {
		return fmt.Errorf("next_action 非法")
	}
	return nil
}

func containsString(value any, want string) bool {
	items, ok := value.([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func schemaDigest(parameters map[string]any) string {
	data, _ := json.Marshal(parameters)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}
func timeout(seconds int) time.Duration {
	if seconds <= 0 {
		return 60 * time.Second
	}
	return time.Duration(seconds) * time.Second
}
func probeBudget(profile string) llm.OutputBudget {
	completionTokens, responseBytes, maxToolCalls := int64(2048), int64(32<<10), int64(1)
	if profile == "v8" {
		maxToolCalls = 16
	}
	if profile == "v9" || profile == "v10" || profile == "v11" || profile == "v12" {
		completionTokens, responseBytes, maxToolCalls = 4096, 48<<10, 16
	}
	return llm.OutputBudget{MaxContentBytes: responseBytes, MaxReasoningBytes: responseBytes,
		MaxExtraFieldBytes: responseBytes, MaxToolNameBytes: 512, MaxToolArgumentsBytes: 16 << 10,
		MaxToolCalls: maxToolCalls, MaxToolArgumentsTotalBytes: 16 << 10,
		MaxResponseBytes: responseBytes, MaxCompletionTokens: completionTokens}
}
