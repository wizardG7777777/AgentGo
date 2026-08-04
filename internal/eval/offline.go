package eval

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agentgo/internal/eval/fakellm"
)

// OfflineRunner 是 V6 §7.6「Offline fake-LLM E2E」层的驱动器：每个 case 起一个
// 脚本化 fake LLM 端点，把被测 agentgo 子进程的 llm.base_url 指向它，复用与
// live run 完全相同的进程外驱动（临时 project_root + /api/input 注入 +
// snapshot 终态轮询），全程离线、零 token 消耗。
//
// 与 live Runner 的差异只在三处：LLM 端点是本地的（配置渲染时覆盖 llm 块）、
// 不跑 preflight（无凭证可查）、不做基线对比（离线指纹与 live 不可比）。
type OfflineRunner struct {
	opts   OfflineOptions
	driver *Runner // 共用子进程驱动原语
}

// OfflineOptions 离线运行器配置。
type OfflineOptions struct {
	SuitePath    string        // 离线套件 YAML（仿 suite.yaml + 每 case 挂 llm_script）
	TemplatePath string        // 配置模板路径（llm 块会被 fake 端点覆盖）
	OnlyCase     string        // 非空 = 只跑指定 case
	BinaryPath   string        // 被测二进制；必填
	RunsDir      string        // 运行现场根目录；空 = eval/runs
	PollInterval time.Duration // 快照轮询间隔；0 = 3s
	Stdout       io.Writer     // 进度输出；nil = io.Discard
}

// NewOfflineRunner 构造离线运行器。
func NewOfflineRunner(opts OfflineOptions) *OfflineRunner {
	return &OfflineRunner{
		opts:   opts,
		driver: NewRunner(RunnerOptions{Stdout: opts.Stdout, PollInterval: opts.PollInterval}),
	}
}

// Run 顺序执行离线套件选中的 case，返回总报告。
func (r *OfflineRunner) Run(ctx context.Context) (*RunReport, error) {
	suite, err := LoadSuite(r.opts.SuitePath)
	if err != nil {
		return nil, err
	}
	runsDir := r.opts.RunsDir
	if runsDir == "" {
		runsDir = filepath.Join("eval", "runs")
	}
	if err := ensureScratchGoMod(filepath.Dir(runsDir)); err != nil {
		return nil, fmt.Errorf("写运行现场 go.mod 挡板失败: %w", err)
	}

	var selected []TaskDef
	for _, t := range suite.Tasks {
		if r.opts.OnlyCase != "" && t.Name != r.opts.OnlyCase {
			continue
		}
		selected = append(selected, t)
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("没有命中任何离线 case（only=%q）", r.opts.OnlyCase)
	}

	rep := &RunReport{
		RunID:     time.Now().Format("20060102-150405") + "-offline",
		Suite:     suite.Name,
		StartedAt: time.Now(),
	}
	r.log("离线套件 %s：%d 个 case 待运行（run %s，fake-LLM 端点）", suite.Name, len(selected), rep.RunID)
	for _, t := range selected {
		rep.Results = append(rep.Results, r.runOne(ctx, suite, rep.RunID, t))
	}
	return rep, nil
}

// runOne 执行单个离线 case 的完整生命周期。
func (r *OfflineRunner) runOne(ctx context.Context, suite *Suite, runID string, task TaskDef) TaskResult {
	started := time.Now()
	res := TaskResult{Name: task.Name}
	res.Metrics.EventCounts = map[string]int{}
	finish := func(status string) TaskResult {
		res.Metrics.TerminalStatus = status
		res.Metrics.WallSec = time.Since(started).Seconds()
		res.Judges = RunJudges(task.Judges, filepath.Join(res.Workdir, "project"), &res.Metrics)
		res.Status = deriveTaskStatus(status, res.Judges)
		r.log("[%s] %s：终态 %s，耗时 %.0fs", strings.ToUpper(res.Status), task.Name, status, res.Metrics.WallSec)
		return res
	}

	// skip / 缺脚本：不执行，直接落类型化结论（不算 pass）
	if task.Skip {
		res.Status = StatusSkipped
		res.Metrics.TerminalStatus = "skipped"
		r.log("[SKIPPED] %s：case 标记 skip", task.Name)
		return res
	}
	if task.LLMScript == "" {
		res.Status = StatusUnqualified
		res.Metrics.TerminalStatus = "unqualified"
		res.Judges = []JudgeResult{{Spec: JudgeSpec{Type: "llm_script"}, Passed: false,
			Status: StatusUnqualified,
			Detail: "offline case 缺少 llm_script（资格不全，不得算 pass）"}}
		r.log("[UNQUALIFIED] %s：缺少 llm_script", task.Name)
		return res
	}
	script, err := fakellm.LoadScript(filepath.Join(suite.Dir, task.LLMScript))
	if err != nil {
		r.log("[错误] %s：LLM 脚本加载失败: %v", task.Name, err)
		return finish("script_error")
	}

	// 1. 运行现场 + fixtures
	runsDir := r.opts.RunsDir
	if runsDir == "" {
		runsDir = filepath.Join("eval", "runs")
	}
	res.Workdir = filepath.Join(runsDir, runID, task.Name)
	projectDir := filepath.Join(res.Workdir, "project")
	if err := ensureDir(projectDir); err != nil {
		r.log("[错误] %s：建运行目录失败: %v", task.Name, err)
		return finish("spawn_error")
	}
	for _, f := range task.Fixtures {
		target := filepath.Join(projectDir, f.Path)
		if err := ensureDir(filepath.Dir(target)); err != nil {
			r.log("[错误] %s：fixture 目录失败: %v", task.Name, err)
			return finish("spawn_error")
		}
		if err := os.WriteFile(target, []byte(f.Content), 0o644); err != nil {
			r.log("[错误] %s：写 fixture %s 失败: %v", task.Name, f.Path, err)
			return finish("spawn_error")
		}
	}

	// 2. 起 fake LLM 端点（随 case 生命周期）
	fake := fakellm.NewServer(script)
	defer fake.Close()

	// 3. 渲染运行配置：llm 块指向 fake 端点
	port, err := freePort()
	if err != nil {
		r.log("[错误] %s：探测空闲端口失败: %v", task.Name, err)
		return finish("spawn_error")
	}
	token := randHex(16)
	cfgPath := filepath.Join(res.Workdir, "config.yaml")
	if err := renderRunConfig(r.opts.TemplatePath, cfgPath, runOverrides{
		ProjectRoot: projectDir,
		WebListen:   fmt.Sprintf("127.0.0.1:%d", port),
		WebToken:    token,
		OfflineLLM:  &offlineLLMOverride{BaseURL: fake.URL(), APIKey: "offline-fake-key", Model: "offline-model"},
	}); err != nil {
		r.log("[错误] %s：渲染配置失败: %v", task.Name, err)
		return finish("spawn_error")
	}

	// 4. 起子进程并驱动到终态（与 live 同一驱动链）
	drive := r.driver.driveChild(ctx, res.Workdir, task.Name, r.opts.BinaryPath, cfgPath, port, token, task.Prompt, task.TimeoutSec)

	// 5. 收割与判据
	events, incomplete := CollectTraceEventsWithStatus(projectDir)
	res.Metrics.TraceEvents = events
	res.Metrics.TraceIncompleteReasons = incomplete
	HarvestEvents(events, &res.Metrics)
	if drive.snap != nil {
		res.Metrics.PromptTokens = drive.snap.SessionPromptTokens
		res.Metrics.CompletionTokens = drive.snap.SessionCompletionTokens
	}
	SnapshotTokenFallback(events, &res.Metrics)
	return finish(drive.status)
}

// log 打印运行进度（nil Stdout 时丢弃）。
func (r *OfflineRunner) log(format string, args ...any) {
	if r.opts.Stdout == nil {
		return
	}
	fmt.Fprintf(r.opts.Stdout, "[eval offline] "+format+"\n", args...)
}
