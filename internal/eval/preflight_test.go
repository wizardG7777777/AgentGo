package eval

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// writeTemplate 在临时目录写一份评测配置模板，baseURL 指向 httptest 服务器。
func writeTemplate(t *testing.T, baseURL string) string {
	t.Helper()
	content := fmt.Sprintf(`llm:
  base_url: %s
  api_key: ${EVAL_TEST_API_KEY}
  default_model: eval-test-model
  timeout_sec: 5
`, baseURL)
	path := filepath.Join(t.TempDir(), "config.template.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写模板失败: %v", err)
	}
	return path
}

// newStubServer 起一个固定状态码/响应体的 LLM 端点桩，返回命中计数与服务器。
func newStubServer(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	hits := &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("探测路径错误: %s", r.URL.Path)
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, hits
}

func TestExtractEnvVars(t *testing.T) {
	vars, err := ExtractEnvVars([]byte("a: ${FOO_KEY}\nb: $BAR\nc: ${FOO_KEY}\nd: 无引用"))
	if err != nil {
		t.Fatalf("YAML 解析失败: %v", err)
	}
	got := strings.Join(vars, ",")
	if got != "BAR,FOO_KEY" {
		t.Fatalf("ExtractEnvVars = %q，期望去重排序后 BAR,FOO_KEY", got)
	}
}

func TestExtractEnvVars_IgnoresComments(t *testing.T) {
	// 注释里的 ${FAKE}、$env:FAKE2 示例不得被误判为变量引用——
	// 真实模板的用法说明文字就含这类字面量（冒烟曾暴露此缺陷）
	tpl := `# 用法：先 export ${FAKE} 再运行
# PowerShell: $env:FAKE2="<你的密钥>"
llm:
  api_key: ${REAL_KEY}   # 行尾注释 $FAKE3 也不算
`
	vars, err := ExtractEnvVars([]byte(tpl))
	if err != nil {
		t.Fatalf("YAML 解析失败: %v", err)
	}
	if len(vars) != 1 || vars[0] != "REAL_KEY" {
		t.Fatalf("ExtractEnvVars = %v，期望仅 [REAL_KEY]（注释免疫）", vars)
	}
}

func TestMissingEnvVars(t *testing.T) {
	lookup := func(name string) (string, bool) {
		switch name {
		case "SET":
			return "v", true
		case "EMPTY":
			return "", true
		default:
			return "", false
		}
	}
	missing := MissingEnvVars([]string{"SET", "EMPTY", "UNSET"}, lookup)
	if len(missing) != 2 || missing[0] != "EMPTY" || missing[1] != "UNSET" {
		t.Fatalf("MissingEnvVars = %v，期望 [EMPTY UNSET]（空串按缺失处理）", missing)
	}
}

func TestPreflight_MissingEnvVar_NoNetwork(t *testing.T) {
	srv, hits := newStubServer(t, http.StatusOK, `{}`)
	tpl := writeTemplate(t, srv.URL)
	// 不注入 EVAL_TEST_API_KEY
	var out strings.Builder
	err := Preflight(context.Background(), PreflightOptions{TemplatePath: tpl}, &out)
	if err == nil {
		t.Fatal("缺环境变量时应失败")
	}
	if !strings.Contains(err.Error(), "EVAL_TEST_API_KEY") || !strings.Contains(err.Error(), "环境变量未注入") {
		t.Fatalf("错误应含缺失变量名与失败类别: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("缺环境变量时不得触碰网络，实际命中 %d 次", hits.Load())
	}
}

func TestPreflight_OK(t *testing.T) {
	srv, _ := newStubServer(t, http.StatusOK, `{"choices":[]}`)
	tpl := writeTemplate(t, srv.URL)
	t.Setenv("EVAL_TEST_API_KEY", "sk-test")
	var out strings.Builder
	if err := Preflight(context.Background(), PreflightOptions{TemplatePath: tpl}, &out); err != nil {
		t.Fatalf("200 应通过: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "凭证检查通过") || !strings.Contains(s, "200 OK") || !strings.Contains(s, "eval-test-model") {
		t.Fatalf("成功报告要素不全: %s", s)
	}
}

// assertHTTPFailure 断言失败警告三要素：HTTP 状态码、端点返回原文、提示。
func assertHTTPFailure(t *testing.T, err error, statusPart, bodyPart, hintPart string) {
	t.Helper()
	if err == nil {
		t.Fatalf("状态 %s 应失败", statusPart)
	}
	s := err.Error()
	if !strings.Contains(s, statusPart) {
		t.Fatalf("警告缺 HTTP 状态码 %q: %s", statusPart, s)
	}
	if !strings.Contains(s, bodyPart) {
		t.Fatalf("警告缺端点返回原文 %q: %s", bodyPart, s)
	}
	if !strings.Contains(s, "提示") || !strings.Contains(s, hintPart) {
		t.Fatalf("警告缺提示信息（含 %q）: %s", hintPart, s)
	}
}

func TestPreflight_Unauthorized401(t *testing.T) {
	srv, _ := newStubServer(t, http.StatusUnauthorized, `{"error":{"message":"Incorrect API key provided"}}`)
	tpl := writeTemplate(t, srv.URL)
	t.Setenv("EVAL_TEST_API_KEY", "sk-bad")
	var out strings.Builder
	err := Preflight(context.Background(), PreflightOptions{TemplatePath: tpl}, &out)
	assertHTTPFailure(t, err, "401 Unauthorized", "Incorrect API key provided", "密钥被端点拒绝")
}

func TestPreflight_Forbidden403(t *testing.T) {
	srv, _ := newStubServer(t, http.StatusForbidden, `{"error":"quota exhausted"}`)
	tpl := writeTemplate(t, srv.URL)
	t.Setenv("EVAL_TEST_API_KEY", "sk-bad")
	var out strings.Builder
	err := Preflight(context.Background(), PreflightOptions{TemplatePath: tpl}, &out)
	assertHTTPFailure(t, err, "403 Forbidden", "quota exhausted", "密钥被端点拒绝")
}

func TestPreflight_NotFound404(t *testing.T) {
	srv, _ := newStubServer(t, http.StatusNotFound, `<html>404 page</html>`)
	tpl := writeTemplate(t, srv.URL)
	t.Setenv("EVAL_TEST_API_KEY", "sk-test")
	var out strings.Builder
	err := Preflight(context.Background(), PreflightOptions{TemplatePath: tpl}, &out)
	assertHTTPFailure(t, err, "404 Not Found", "404 page", "base_url")
}

func TestPreflight_BadRequest400(t *testing.T) {
	srv, _ := newStubServer(t, http.StatusBadRequest, `{"error":"model not found"}`)
	tpl := writeTemplate(t, srv.URL)
	t.Setenv("EVAL_TEST_API_KEY", "sk-test")
	var out strings.Builder
	err := Preflight(context.Background(), PreflightOptions{TemplatePath: tpl}, &out)
	assertHTTPFailure(t, err, "400 Bad Request", "model not found", "default_model")
}

func TestPreflight_ServerError500(t *testing.T) {
	srv, _ := newStubServer(t, http.StatusInternalServerError, `internal error`)
	tpl := writeTemplate(t, srv.URL)
	t.Setenv("EVAL_TEST_API_KEY", "sk-test")
	var out strings.Builder
	err := Preflight(context.Background(), PreflightOptions{TemplatePath: tpl}, &out)
	assertHTTPFailure(t, err, "500 Internal Server Error", "internal error", "稍后重试")
}

func TestPreflight_RateLimited429_IsWarning(t *testing.T) {
	srv, _ := newStubServer(t, http.StatusTooManyRequests, `{"error":"slow down"}`)
	tpl := writeTemplate(t, srv.URL)
	t.Setenv("EVAL_TEST_API_KEY", "sk-test")
	var out strings.Builder
	if err := Preflight(context.Background(), PreflightOptions{TemplatePath: tpl}, &out); err != nil {
		t.Fatalf("429 应视为非致命警告: %v", err)
	}
	if !strings.Contains(out.String(), "429") || !strings.Contains(out.String(), "警告") {
		t.Fatalf("429 应在成功报告中带警告: %s", out.String())
	}
}

func TestPreflight_NetworkError(t *testing.T) {
	srv, _ := newStubServer(t, http.StatusOK, `{}`)
	srv.Close() // 立即关闭，模拟不可达
	tpl := writeTemplate(t, srv.URL)
	t.Setenv("EVAL_TEST_API_KEY", "sk-test")
	var out strings.Builder
	err := Preflight(context.Background(), PreflightOptions{TemplatePath: tpl}, &out)
	if err == nil || !strings.Contains(err.Error(), "无法连接端点") {
		t.Fatalf("断网应报连接类提示: %v", err)
	}
}

func TestPreflight_BodyTruncation(t *testing.T) {
	long := strings.Repeat("x", probeBodyMaxLen+500)
	srv, _ := newStubServer(t, http.StatusUnauthorized, long)
	tpl := writeTemplate(t, srv.URL)
	t.Setenv("EVAL_TEST_API_KEY", "sk-bad")
	var out strings.Builder
	err := Preflight(context.Background(), PreflightOptions{TemplatePath: tpl}, &out)
	if err == nil {
		t.Fatal("401 应失败")
	}
	if !strings.Contains(err.Error(), "（已截断）") {
		t.Fatalf("超长端点返回应截断并带标记: %d 字节", len(err.Error()))
	}
}

func TestPreflight_EmptyAPIKeyAfterExpansion(t *testing.T) {
	// 模板 api_key 为字面量空串：环境变量齐全也救不了，应字段级报错
	srv, hits := newStubServer(t, http.StatusOK, `{}`)
	content := fmt.Sprintf("llm:\n  base_url: %s\n  api_key: \"\"\n  default_model: m\n", srv.URL)
	tpl := filepath.Join(t.TempDir(), "tpl.yaml")
	if err := os.WriteFile(tpl, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	err := Preflight(context.Background(), PreflightOptions{TemplatePath: tpl}, &out)
	if err == nil || !strings.Contains(err.Error(), "api_key 展开后为空") {
		t.Fatalf("空 api_key 应字段级报错: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("字段缺失时不得触碰网络，实际命中 %d 次", hits.Load())
	}
}

func TestPreflight_SoftMissingEnvVar_WarningOnly(t *testing.T) {
	// llm 块外的 ${VAR}（如 search_api_key）缺失只降级、不致命——
	// 黄金任务不依赖 web 搜索，不能因此拦住整个跑批
	srv, hits := newStubServer(t, http.StatusOK, `{}`)
	content := fmt.Sprintf(`llm:
  base_url: %s
  api_key: sk-plaintext
  default_model: m
search_api_key: ${EVAL_TEST_SEARCH_UNSET}
`, srv.URL)
	tpl := filepath.Join(t.TempDir(), "tpl.yaml")
	if err := os.WriteFile(tpl, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := Preflight(context.Background(), PreflightOptions{TemplatePath: tpl}, &out); err != nil {
		t.Fatalf("软缺变量不应致命: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "EVAL_TEST_SEARCH_UNSET") || !strings.Contains(s, "不影响评测") {
		t.Fatalf("成功报告应含软缺警告: %s", s)
	}
	if hits.Load() != 1 {
		t.Fatalf("软缺路径应完成一次真实探测，实际命中 %d 次", hits.Load())
	}
}

func TestPreflight_TemplateNotFound(t *testing.T) {
	var out strings.Builder
	err := Preflight(context.Background(), PreflightOptions{TemplatePath: filepath.Join(t.TempDir(), "不存在.yaml")}, &out)
	if err == nil || !strings.Contains(err.Error(), "无法读取评测配置模板") {
		t.Fatalf("模板缺失应报错: %v", err)
	}
}

func TestPreflight_TemplateFailsV4Validate(t *testing.T) {
	// agents[0] 缺行为参数（agent_max_loops 等必填且 >0）——
	// 必须在 preflight 阶段拦截，而不是等子进程启动失败空转 90 秒
	srv, hits := newStubServer(t, http.StatusOK, `{}`)
	content := fmt.Sprintf(`llm:
  base_url: %s
  api_key: ${EVAL_TEST_API_KEY}
  default_model: m
agents:
  - kind: worker
    replicas: 1
    tools: [read_file]
`, srv.URL)
	tpl := filepath.Join(t.TempDir(), "tpl.yaml")
	if err := os.WriteFile(tpl, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EVAL_TEST_API_KEY", "sk-test")
	var out strings.Builder
	err := Preflight(context.Background(), PreflightOptions{TemplatePath: tpl}, &out)
	if err == nil || !strings.Contains(err.Error(), "v4 配置校验") || !strings.Contains(err.Error(), "agent_max_loops") {
		t.Fatalf("模板未过 v4 校验应报错并指到具体规则: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("校验失败时不得触碰网络，实际命中 %d 次", hits.Load())
	}
}

func TestCLI_ExitCodes(t *testing.T) {
	var out, errOut strings.Builder
	if code := CLI([]string{"不存在的子命令"}, &out, &errOut); code != 2 {
		t.Fatalf("未知子命令应退出 2，实际 %d", code)
	}
	if code := CLI(nil, &out, &errOut); code != 2 {
		t.Fatalf("空参数应退出 2，实际 %d", code)
	}
	// run 在缺环境变量时应被 preflight 拦住：退出 1 且报凭证检查失败
	errOut.Reset()
	tpl := writeTemplate(t, "http://127.0.0.1:1") // 引用 ${EVAL_TEST_API_KEY}，此处不注入
	if code := CLI([]string{"run", "-template", tpl, "-suite", filepath.Join(t.TempDir(), "无.yaml")}, &out, &errOut); code != 1 {
		t.Fatalf("缺环境变量时 run 应退出 1，实际 %d（stderr: %s）", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "凭证检查失败") {
		t.Fatalf("run 的失败应来自 preflight: %s", errOut.String())
	}
}

func TestCLI_Preflight_EndToEnd(t *testing.T) {
	// 缺变量 → 1
	srv, _ := newStubServer(t, http.StatusOK, `{}`)
	tpl := writeTemplate(t, srv.URL)
	var out, errOut strings.Builder
	if code := CLI([]string{"preflight", "-template", tpl}, &out, &errOut); code != 1 {
		t.Fatalf("缺变量应退出 1，实际 %d（stderr: %s）", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "EVAL_TEST_API_KEY") {
		t.Fatalf("stderr 应含缺失变量名: %s", errOut.String())
	}
	// 注入后 → 0
	t.Setenv("EVAL_TEST_API_KEY", "sk-test")
	out.Reset()
	errOut.Reset()
	if code := CLI([]string{"preflight", "-template", tpl}, &out, &errOut); code != 0 {
		t.Fatalf("凭证有效应退出 0，实际 %d（stderr: %s）", code, errOut.String())
	}
}
