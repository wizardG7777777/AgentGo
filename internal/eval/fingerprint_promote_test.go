package eval

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chdirTemp 把测试切到临时目录（promote/record 用包级默认相对路径），并注册恢复。
func chdirTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	return dir
}

func testEnv() Environment {
	return Environment{
		Model: "m1", PromptSHA256: "p1", ConfigSHA256: "c1", AgentGoCommit: "aaa",
		APIProtocol: apiProtocolV6, EndpointHash: "e1", GraphSchema: graphSchemaV6,
		SuiteDigest: "s1", OS: "windows", Arch: "amd64",
	}
}

func TestComparabilityDiffs(t *testing.T) {
	base := testEnv()
	if diffs := base.comparabilityDiffs(base); len(diffs) != 0 {
		t.Fatalf("相同指纹应无可比性差异，got %v", diffs)
	}
	other := base
	other.Model = "m2"
	other.SuiteDigest = "s2"
	diffs := base.comparabilityDiffs(other)
	if len(diffs) != 2 || diffs[0] != "model" || diffs[1] != "suite_digest" {
		t.Fatalf("差异项 = %v，期望 [model suite_digest]", diffs)
	}
}

func TestCompare_NotComparableOnKeyMismatch(t *testing.T) {
	base := &Baseline{Environment: testEnv(), Tasks: map[string]BaselineTask{}}
	other := testEnv()
	other.AgentGoCommit = "bbb" // commit 不同不破坏可比性
	alerts, err := base.Compare(other, nil, compareBand)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if len(alerts) != 1 || alerts[0].Level != "info" || !strings.Contains(alerts[0].Detail, "agentgo_commit") {
		t.Fatalf("commit 差异应只产生 info 告警，got %+v", alerts)
	}

	other.Model = "m2"
	alerts, err = base.Compare(other, nil, compareBand)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if len(alerts) != 1 || alerts[0].Level != "not_comparable" || !strings.Contains(alerts[0].Detail, "model") {
		t.Fatalf("model 差异应报 not_comparable 且列差异项，got %+v", alerts)
	}
}

func TestFingerprintEnvironment_V6Fields(t *testing.T) {
	dir := chdirTemp(t)
	tpl := `llm:
  base_url: http://localhost:9999
  api_key: dummy
  default_model: test-model
agents: []
`
	if err := os.WriteFile(filepath.Join(dir, "tpl.yaml"), []byte(tpl), 0o644); err != nil {
		t.Fatal(err)
	}
	env, err := FingerprintEnvironment("tpl.yaml")
	if err != nil {
		t.Fatalf("FingerprintEnvironment: %v", err)
	}
	if env.APIProtocol != apiProtocolV6 || env.GraphSchema != graphSchemaV6 {
		t.Fatalf("协议/schema 缺失: %+v", env)
	}
	if env.EndpointHash == "" || env.OS == "" || env.Arch == "" {
		t.Fatalf("endpoint/os/arch 缺失: %+v", env)
	}
	if strings.Contains(env.Model, "api_key") {
		t.Fatalf("指纹不得包含密钥材料")
	}
}

func TestFingerprintEnvironment_UsesExpandedEffectiveConfig(t *testing.T) {
	dir := chdirTemp(t)
	tpl := `llm:
  base_url: ${AGENTGO_EVAL_FP_BASE_URL}
  api_key: ${AGENTGO_EVAL_FP_API_KEY}
  default_model: ${AGENTGO_EVAL_FP_DEFAULT_MODEL}
scheduler:
  model: ${AGENTGO_EVAL_FP_SCHEDULER_MODEL}
agents: []
ui:
  web:
    token: ${AGENTGO_EVAL_FP_WEB_TOKEN}
`
	if err := os.WriteFile(filepath.Join(dir, "tpl.yaml"), []byte(tpl), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTGO_EVAL_FP_BASE_URL", "https://endpoint-a.example/v1")
	t.Setenv("AGENTGO_EVAL_FP_API_KEY", "sk-first")
	t.Setenv("AGENTGO_EVAL_FP_DEFAULT_MODEL", "worker-default")
	t.Setenv("AGENTGO_EVAL_FP_SCHEDULER_MODEL", "scheduler-a")
	t.Setenv("AGENTGO_EVAL_FP_WEB_TOKEN", "web-first")

	baseEnv, err := FingerprintEnvironment("tpl.yaml")
	if err != nil {
		t.Fatalf("FingerprintEnvironment: %v", err)
	}
	if baseEnv.Model != "scheduler-a" {
		t.Fatalf("Model = %q，期望实际 Scheduler 覆盖模型 scheduler-a", baseEnv.Model)
	}

	// 密钥在展开后也必须脱敏，轮换 API key / Web token 不改变配置指纹。
	t.Setenv("AGENTGO_EVAL_FP_API_KEY", "sk-rotated")
	t.Setenv("AGENTGO_EVAL_FP_WEB_TOKEN", "web-rotated")
	secretRotated, err := FingerprintEnvironment("tpl.yaml")
	if err != nil {
		t.Fatalf("FingerprintEnvironment after secret rotation: %v", err)
	}
	if secretRotated.ConfigSHA256 != baseEnv.ConfigSHA256 {
		t.Fatalf("密钥轮换不应改变 config digest: %s vs %s", baseEnv.ConfigSHA256, secretRotated.ConfigSHA256)
	}

	// 非秘密 endpoint 经环境变量展开后属于可比性键，变化必须失去可比性。
	t.Setenv("AGENTGO_EVAL_FP_BASE_URL", "https://endpoint-b.example/v1")
	endpointChanged, err := FingerprintEnvironment("tpl.yaml")
	if err != nil {
		t.Fatalf("FingerprintEnvironment after endpoint change: %v", err)
	}
	if endpointChanged.EndpointHash == baseEnv.EndpointHash || endpointChanged.ConfigSHA256 == baseEnv.ConfigSHA256 {
		t.Fatalf("endpoint 变化应改变 endpoint/config 指纹: base=%+v changed=%+v", baseEnv, endpointChanged)
	}
	baseline := &Baseline{Environment: baseEnv, Tasks: map[string]BaselineTask{}}
	alerts, err := baseline.Compare(endpointChanged, nil, compareBand)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if len(alerts) != 1 || alerts[0].Level != "not_comparable" ||
		!strings.Contains(alerts[0].Detail, "endpoint_hash") || !strings.Contains(alerts[0].Detail, "config_digest") {
		t.Fatalf("展开后的 endpoint 变化应报 not_comparable: %+v", alerts)
	}

	// Scheduler 覆盖模型同样按实际请求模型记录，而不是 llm.default_model。
	t.Setenv("AGENTGO_EVAL_FP_BASE_URL", "https://endpoint-a.example/v1")
	t.Setenv("AGENTGO_EVAL_FP_SCHEDULER_MODEL", "scheduler-b")
	modelChanged, err := FingerprintEnvironment("tpl.yaml")
	if err != nil {
		t.Fatalf("FingerprintEnvironment after model change: %v", err)
	}
	if modelChanged.Model != "scheduler-b" {
		t.Fatalf("Model = %q，期望 scheduler-b", modelChanged.Model)
	}
	alerts, err = baseline.Compare(modelChanged, nil, compareBand)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if len(alerts) != 1 || alerts[0].Level != "not_comparable" || !strings.Contains(alerts[0].Detail, "model") {
		t.Fatalf("展开后的 Scheduler 模型变化应报 not_comparable: %+v", alerts)
	}
}

func TestPromoteCLI(t *testing.T) {
	var out, errOut bytes.Buffer

	// 无候选 → 拒绝
	chdirTemp(t)
	if code := promoteCLI(nil, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "不存在基线候选") {
		t.Fatalf("无候选应拒绝: code=%d err=%s", code, errOut.String())
	}

	// 含失败任务的候选 → 拒绝晋升且不写 accepted
	bad := &Baseline{Version: 1, Environment: testEnv(), Tasks: map[string]BaselineTask{
		"t1": {JudgesPassed: true},
		"t2": {JudgesPassed: false},
	}}
	if err := SaveBaseline(defaultCandidatePath, bad); err != nil {
		t.Fatal(err)
	}
	errOut.Reset()
	if code := promoteCLI(nil, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "t2") {
		t.Fatalf("失败候选应拒绝且点名: code=%d err=%s", code, errOut.String())
	}
	if _, err := os.Stat(defaultBaselinePath); !os.IsNotExist(err) {
		t.Fatalf("失败候选不得写 accepted baseline")
	}

	// 全通过候选 → 晋升成功，accepted 落盘
	good := &Baseline{Version: 1, Environment: testEnv(), Tasks: map[string]BaselineTask{
		"t1": {JudgesPassed: true},
		"t2": {JudgesPassed: true},
	}}
	if err := SaveBaseline(defaultCandidatePath, good); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := promoteCLI(nil, &out, &errOut); code != 0 {
		t.Fatalf("全通过候选应晋升成功: code=%d err=%s", code, errOut.String())
	}
	got, err := LoadBaseline(defaultBaselinePath)
	if err != nil || got == nil || len(got.Tasks) != 2 {
		t.Fatalf("accepted baseline 未正确落盘: %+v err=%v", got, err)
	}
}
