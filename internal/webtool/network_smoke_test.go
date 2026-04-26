package webtool

// 真网络冒烟测试——只在显式开启时运行，避免 CI 因 DDG / example.com 抖动变红。
//
// 用法：
//
//	AGENTGO_NETWORK_TEST=1 go test -v -run TestNetwork ./internal/webtool/
//
// 设计取舍：
//   - 默认 skip：常规 `go test ./...` 不应触网，否则 DDG 偶发 bot 检测会让 CI 红
//   - 不用 build tag：env 门控比 build tag 更易发现（IDE 一眼可见），适合"想验证就跑"
//   - 不 mock：mock 网络层会绕开真实失败模式（DDG HTML 解析破坏 / SSRF / TLS 握手），
//     这些恰恰是 webtool 最容易出 bug 的地方。env 门控让它"想跑能跑、不想跑不烦人"
//
// 当 DDG / Serper / Tavily 抖动或 ban 时，本测试会红——这是预期行为：
// 失败信号意味着"应当切到备用 provider 或修 prompt 让 LLM 别频繁触网"，
// 而不是悄悄把测试改成"无论如何都返回 ok"。

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

const networkTestEnvVar = "AGENTGO_NETWORK_TEST"

func skipIfNoNetwork(t *testing.T) {
	t.Helper()
	if os.Getenv(networkTestEnvVar) != "1" {
		t.Skipf("跳过网络测试；执行 `%s=1 go test -run %s` 启用", networkTestEnvVar, t.Name())
	}
}

// TestNetwork_DuckDuckGo_Search 验证 DDG HTML provider 能完成一次真实搜索并返回结果。
// 这是 web_search 工具默认 backend——bootstrap fallback 链终点必须工作。
func TestNetwork_DuckDuckGo_Search(t *testing.T) {
	skipIfNoNetwork(t)

	p := &DuckDuckGoProvider{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	results, err := p.Search(ctx, "golang programming language", &SearchOptions{NumResults: 5})
	if err != nil {
		t.Fatalf("DDG 搜索失败: %v", err)
	}
	if len(results) == 0 {
		t.Fatalf("DDG 返回 0 条结果——HTML 解析可能因页面结构变化已失效")
	}
	for i, r := range results {
		if r.Title == "" {
			t.Errorf("结果 %d 标题为空", i)
		}
		if !strings.HasPrefix(r.URL, "http://") && !strings.HasPrefix(r.URL, "https://") {
			t.Errorf("结果 %d URL 不是 http(s)：%q", i, r.URL)
		}
	}
	t.Logf("DDG 返回 %d 条结果，首条：%q -> %s", len(results), results[0].Title, results[0].URL)
}

// TestNetwork_FetchURL_Auto 验证 web_fetch 工具能抓取一个稳定 URL 并返回非空文本。
// example.com 是 IANA 维护的"永远在线"测试域，没有 bot 检测、没有地区限制。
func TestNetwork_FetchURL_Auto(t *testing.T) {
	skipIfNoNetwork(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	content, err := FetchURLWithMode(ctx, "https://example.com", "auto")
	if err != nil {
		t.Fatalf("FetchURL 失败: %v", err)
	}
	if content == "" {
		t.Fatalf("FetchURL 返回空内容")
	}
	// example.com 主页固定有 "Example Domain" 字样
	if !strings.Contains(content, "Example Domain") {
		t.Errorf("内容不含 'Example Domain'——可能被 redirect 或代理拦截。前 200 字：%q",
			content[:min(200, len(content))])
	}
	t.Logf("Fetch 成功，返回 %d 字符", len(content))
}

// TestNetwork_FetchURL_SSRF 验证 web_fetch 对 SSRF 防御仍然生效（loopback 拒绝）。
// 这条不依赖外部网络，但放在网络测试组里因为 webtool 主要是网络代码，集中维护。
func TestNetwork_FetchURL_SSRF(t *testing.T) {
	skipIfNoNetwork(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := FetchURLWithMode(ctx, "http://127.0.0.1/", "auto")
	if err == nil {
		t.Fatalf("SSRF 防御失效：访问 127.0.0.1 应被拒绝")
	}
	t.Logf("SSRF 拦截生效：%v", err)
}
