package eval

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Runner 是黄金任务的进程外黑盒驱动器：为每个任务起真实 agentgo 子进程
// （隔离的临时 project_root + 随机端口/令牌的 web 前端），经 /api/input
// 注入提示词，轮询 /api/snapshot 判定终态，收割 trace 并执行判据。
type Runner struct {
	opts   RunnerOptions
	client *http.Client
}

// RunnerOptions 运行器配置。
type RunnerOptions struct {
	SuitePath    string        // 套件 YAML 路径
	TemplatePath string        // 配置模板路径（llm 块含 ${VAR} 占位符）
	OnlyTask     string        // 非空 = 只跑指定任务
	SmokeOnly    bool          // true = 只跑 smoke 标记的任务
	BinaryPath   string        // 被测二进制；空 = os.Executable()
	RunsDir      string        // 运行现场根目录；空 = eval/runs
	PollInterval time.Duration // 快照轮询间隔；0 = 3s
	Stdout       io.Writer     // 进度输出；nil = io.Discard
}

// NewRunner 构造运行器。
func NewRunner(opts RunnerOptions) *Runner {
	return &Runner{opts: opts, client: &http.Client{Timeout: 15 * time.Second}}
}

// pollSnapshot 是 /api/snapshot 的最小解码结构——只取终态判定与 token 读数
// 需要的字段，其余字段（轮次、feed 等大对象）不解析以省内存。
type pollSnapshot struct {
	Agents []struct {
		State string `json:"state"`
	} `json:"agents"`
	Tasks []struct {
		Status string `json:"status"`
	} `json:"tasks"`
	PendingInteractions     []json.RawMessage `json:"pending_interactions"`
	SessionPromptTokens     int64             `json:"session_prompt_tokens"`
	SessionCompletionTokens int64             `json:"session_completion_tokens"`
	SessionCallCount        int               `json:"session_call_count"`
}

// snapshotQuiet 报告系统是否完全静止：无代理在跑、无任务在途、无待答交互。
func snapshotQuiet(s *pollSnapshot) bool {
	for _, a := range s.Agents {
		if a.State != "idle" {
			return false
		}
	}
	for _, t := range s.Tasks {
		if t.Status == "pending" || t.Status == "processing" {
			return false
		}
	}
	return len(s.PendingInteractions) == 0
}

// snapshotActivity 报告系统是否动过：防止「注入后第一个轮询间隙全静止」
// 被误判为收敛——必须先观察到活动痕迹，再承认静止是终态。
func snapshotActivity(s *pollSnapshot) bool {
	if len(s.Tasks) > 0 || s.SessionCallCount > 0 {
		return true
	}
	for _, a := range s.Agents {
		if a.State != "idle" {
			return true
		}
	}
	return false
}

// Run 顺序执行套件中选中的任务（v1 不并行：避免端口/配额竞争干扰指标），
// 返回总报告。套件加载或环境指纹失败属用法错误，直接返回 error。
func (r *Runner) Run(ctx context.Context) (*RunReport, error) {
	suite, err := LoadSuite(r.opts.SuitePath)
	if err != nil {
		return nil, err
	}
	env, err := FingerprintEnvironment(r.opts.TemplatePath)
	if err != nil {
		return nil, err
	}
	// 运行现场会落代理写出的 .go 文件（fixtures/产物），若被父模块的
	// go build/test ./... 走查会直接编挂（2026-07-29 实测：run 现场的
	// 调研 fixture 让全量测试失败）。确保现场根有隔离 go.mod 挡板。
	runsDir := r.opts.RunsDir
	if runsDir == "" {
		runsDir = filepath.Join("eval", "runs")
	}
	if err := ensureScratchGoMod(filepath.Dir(runsDir)); err != nil {
		return nil, fmt.Errorf("写运行现场 go.mod 挡板失败: %w", err)
	}

	var selected []TaskDef
	for _, t := range suite.Tasks {
		if r.opts.OnlyTask != "" && t.Name != r.opts.OnlyTask {
			continue
		}
		if r.opts.SmokeOnly && !t.Smoke {
			continue
		}
		selected = append(selected, t)
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("没有命中任何任务（only=%q smoke=%v）", r.opts.OnlyTask, r.opts.SmokeOnly)
	}

	rep := &RunReport{
		RunID:       time.Now().Format("20060102-150405"),
		Suite:       suite.Name,
		StartedAt:   time.Now(),
		Environment: env,
	}
	r.log("套件 %s：%d 个任务待运行（run %s）", suite.Name, len(selected), rep.RunID)
	for _, t := range selected {
		rep.Results = append(rep.Results, r.runOne(ctx, rep.RunID, t))
	}
	return rep, nil
}

// runOne 执行单个黄金任务的完整生命周期。
// 任何阶段失败都收敛为 TaskResult（终态 + 现场），不让一个任务炸掉整个套件。
func (r *Runner) runOne(ctx context.Context, runID string, task TaskDef) TaskResult {
	started := time.Now()
	res := TaskResult{Name: task.Name}
	res.Metrics.EventCounts = map[string]int{}
	finish := func(status string) TaskResult {
		res.Metrics.TerminalStatus = status
		res.Metrics.WallSec = time.Since(started).Seconds()
		res.Judges = RunJudges(task.Judges, filepath.Join(res.Workdir, "project"), &res.Metrics)
		res.Passed = true
		for _, j := range res.Judges {
			if !j.Passed {
				res.Passed = false
				break
			}
		}
		mark := "PASS"
		if !res.Passed {
			mark = "FAIL"
		}
		r.log("[%s] %s：终态 %s，耗时 %.0fs", mark, task.Name, status, res.Metrics.WallSec)
		return res
	}

	// 1. 运行现场：eval/runs/<runID>/<task>/project（临时 project_root）
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

	// 2. 渲染运行配置（保留 ${VAR} 占位符，覆盖 project_root/web 三项）
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
	}); err != nil {
		r.log("[错误] %s：渲染配置失败: %v", task.Name, err)
		return finish("spawn_error")
	}

	// 3. 起子进程（stdout/stderr 落 child.log；env 继承——密钥经环境变量进子进程）
	binary := r.opts.BinaryPath
	if binary == "" {
		if exe, err := os.Executable(); err == nil {
			binary = exe
		}
	}
	childCtx, cancelChild := context.WithCancel(ctx)
	defer cancelChild()
	cmd := exec.CommandContext(childCtx, binary, "-config", cfgPath, "-skip-startup-probe")
	logFile, err := os.Create(filepath.Join(res.Workdir, "child.log"))
	if err != nil {
		r.log("[错误] %s：建 child.log 失败: %v", task.Name, err)
		return finish("spawn_error")
	}
	defer logFile.Close()
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		r.log("[错误] %s：子进程启动失败: %v", task.Name, err)
		return finish("spawn_error")
	}
	// 子进程退出监视（健康检查与轮询都要能区分「系统静止」与「进程死了」）
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	r.log("%s：子进程已起（pid %d，端口 %d）", task.Name, cmd.Process.Pid, port)

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	stopChild := func() {
		cancelChild()
		select {
		case <-exited:
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
	}

	// 4. 健康等待
	if !r.waitHealthy(ctx, baseURL, exited, 90*time.Second) {
		select {
		case err := <-exited:
			r.log("[错误] %s：子进程健康等待期内退出: %v", task.Name, err)
		default:
			r.log("[错误] %s：健康等待超时", task.Name)
		}
		r.logChildLogTail(res.Workdir, 2000)
		stopChild()
		return finish("health_timeout")
	}

	// 5. 注入提示词
	if err := r.postInput(ctx, baseURL, token, task.Prompt); err != nil {
		r.log("[错误] %s：注入失败: %v", task.Name, err)
		stopChild()
		return finish("inject_error")
	}
	r.log("%s：提示词已注入，等待收敛（超时 %ds）…", task.Name, task.TimeoutSec)

	// 6. 终态等待
	status, snap := r.pollTerminal(ctx, baseURL, token, time.Duration(task.TimeoutSec)*time.Second, exited)
	stopChild()

	// 7. 收割与判据
	events := CollectTraceEvents(projectDir)
	HarvestEvents(events, &res.Metrics)
	if snap != nil {
		res.Metrics.PromptTokens = snap.SessionPromptTokens
		res.Metrics.CompletionTokens = snap.SessionCompletionTokens
	}
	SnapshotTokenFallback(events, &res.Metrics)
	return finish(status)
}

// waitHealthy 轮询 /healthz 直到就绪或超时；子进程提前退出立即返回 false。
func (r *Runner) waitHealthy(ctx context.Context, baseURL string, exited chan error, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		case <-exited:
			return false
		default:
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/healthz", nil)
		resp, err := r.client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// postInput 经 /api/input 注入任务提示词（Bearer 鉴权）。
func (r *Runner) postInput(ctx context.Context, baseURL, token, prompt string) error {
	body, _ := json.Marshal(map[string]string{"text": prompt})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/input", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		text, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(text))
	}
	return nil
}

// pollTerminal 轮询快照直到系统收敛（静止且动过，连续 3 次）或超时。
// 返回终态与最后一次快照（token 读数来源）。
func (r *Runner) pollTerminal(ctx context.Context, baseURL, token string, timeout time.Duration, exited chan error) (string, *pollSnapshot) {
	interval := r.opts.PollInterval
	if interval <= 0 {
		interval = 3 * time.Second
	}
	deadline := time.Now().Add(timeout)
	quietStreak := 0
	var last *pollSnapshot
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "cancelled", last
		case <-exited:
			return "child_exited", last
		default:
		}
		if snap, err := r.fetchSnapshot(ctx, baseURL, token); err == nil {
			last = snap
			if snapshotQuiet(snap) && snapshotActivity(snap) {
				quietStreak++
				if quietStreak >= 3 {
					return "completed", snap
				}
			} else {
				quietStreak = 0
			}
		}
		time.Sleep(interval)
	}
	return "timeout", last
}

// fetchSnapshot 拉取一次 /api/snapshot（Bearer 鉴权，最小解码）。
func (r *Runner) fetchSnapshot(ctx context.Context, baseURL, token string) (*pollSnapshot, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/snapshot", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var snap pollSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// runOverrides 是按任务覆盖进渲染配置的三个运行时字段。
type runOverrides struct {
	ProjectRoot string
	WebListen   string
	WebToken    string
}

// renderRunConfig 以模板为底渲染运行配置：解析为 YAML 节点值（${VAR}
// 占位符作为普通字符串原样保留，由子进程启动时 ExpandEnv 展开），
// 只覆盖 project_root 与 ui.web 两个域。
func renderRunConfig(templatePath, outPath string, ov runOverrides) error {
	data, err := os.ReadFile(templatePath)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("模板 YAML 解析失败: %w", err)
	}
	doc["project_root"] = filepath.ToSlash(ov.ProjectRoot)

	ui, _ := doc["ui"].(map[string]any)
	if ui == nil {
		ui = map[string]any{}
		doc["ui"] = ui
	}
	ui["frontends"] = []any{"web"}
	web, _ := ui["web"].(map[string]any)
	if web == nil {
		web = map[string]any{}
		ui["web"] = web
	}
	web["listen"] = ov.WebListen
	web["token"] = ov.WebToken

	out, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	return os.WriteFile(outPath, out, 0o600) // 0600：渲染产物含 web token
}

// freePort 探测一个空闲 loopback 端口（监听即关，竞态窗口可接受）。
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// randHex 生成 n 字节的随机 hex 字符串（web token 用）。
func randHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

// log 打印运行进度（nil Stdout 时丢弃）。
func (r *Runner) log(format string, args ...any) {
	if r.opts.Stdout == nil {
		return
	}
	fmt.Fprintf(r.opts.Stdout, "[eval] "+format+"\n", args...)
}

// logChildLogTail 把运行现场 child.log 的尾部打到进度输出——子进程启动
// 失败时用户不必手动翻文件就能看到直接死因（如配置校验拒绝）。
func (r *Runner) logChildLogTail(workdir string, maxBytes int64) {
	if r.opts.Stdout == nil {
		return
	}
	path := filepath.Join(workdir, "child.log")
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return
	}
	if int64(len(data)) > maxBytes {
		data = data[int64(len(data))-maxBytes:]
	}
	fmt.Fprintf(r.opts.Stdout, "[eval] ---- child.log 尾部 ----\n%s\n[eval] ---- 以上 ----\n", strings.TrimSpace(string(data)))
}

// ensureScratchGoMod 确保运行现场根目录存在隔离 go.mod：运行现场会落
// 代理写出的 .go 文件（fixtures/产物），没有这层挡板时父模块的
// go build/test ./... 会走查现场并把不完整的产物代码编挂。
// 已存在（或父级已是其它模块）时不覆盖。
func ensureScratchGoMod(dir string) error {
	if dir == "" || dir == "." {
		return nil
	}
	path := filepath.Join(dir, "go.mod")
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	return os.WriteFile(path, []byte("module agentgoevalscratch\n\ngo 1.25\n"), 0o644)
}
