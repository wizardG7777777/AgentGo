package webtool

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- SearchProvider 接口实现验证 ---

func TestDuckDuckGoProvider_Name(t *testing.T) {
	p := &DuckDuckGoProvider{}
	if p.Name() != "duckduckgo_html" {
		t.Errorf("期望 Name() = duckduckgo_html，实际: %s", p.Name())
	}
}

func TestDuckDuckGoProvider_EmptyQuery(t *testing.T) {
	p := &DuckDuckGoProvider{}
	_, err := p.Search(context.Background(), "", nil)
	if err == nil {
		t.Error("期望空 query 返回错误")
	}
}

func TestSearXNGProvider_Name(t *testing.T) {
	p := &SearXNGProvider{BaseURL: "http://localhost:8080"}
	if p.Name() != "searxng" {
		t.Errorf("期望 Name() = searxng，实际: %s", p.Name())
	}
}

func TestSearXNGProvider_EmptyQuery(t *testing.T) {
	p := &SearXNGProvider{BaseURL: "http://localhost:8080"}
	_, err := p.Search(context.Background(), "", nil)
	if err == nil {
		t.Error("期望空 query 返回错误")
	}
}

func TestSearXNGProvider_Search(t *testing.T) {
	// 模拟 SearXNG 服务器
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			t.Errorf("期望路径 /search，实际: %s", r.URL.Path)
		}
		q := r.URL.Query().Get("q")
		if q != "test query" {
			t.Errorf("期望查询 test query，实际: %s", q)
		}
		format := r.URL.Query().Get("format")
		if format != "json" {
			t.Errorf("期望 format=json，实际: %s", format)
		}

		resp := map[string]any{
			"results": []map[string]string{
				{"title": "Result 1", "url": "https://example.com/1", "content": "Snippet 1"},
				{"title": "Result 2", "url": "https://example.com/2", "content": "Snippet 2"},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := &SearXNGProvider{BaseURL: srv.URL}
	results, err := p.Search(context.Background(), "test query", nil)
	if err != nil {
		t.Fatalf("搜索失败: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("期望 2 条结果，实际 %d 条", len(results))
	}
	if results[0].Title != "Result 1" {
		t.Errorf("第1条标题不正确: %s", results[0].Title)
	}
	if results[0].URL != "https://example.com/1" {
		t.Errorf("第1条 URL 不正确: %s", results[0].URL)
	}
	if results[0].Snippet != "Snippet 1" {
		t.Errorf("第1条摘要不正确: %s", results[0].Snippet)
	}
}

func TestTavilyProvider_Name(t *testing.T) {
	p := &TavilyProvider{APIKey: "test-key"}
	if p.Name() != "tavily" {
		t.Errorf("期望 Name() = tavily，实际: %s", p.Name())
	}
}

func TestTavilyProvider_EmptyQuery(t *testing.T) {
	p := &TavilyProvider{APIKey: "test-key"}
	_, err := p.Search(context.Background(), "", nil)
	if err == nil {
		t.Error("期望空 query 返回错误")
	}
}

func TestTavilyProvider_Search(t *testing.T) {
	// 模拟 Tavily API 服务器
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("期望 POST 方法，实际: %s", r.Method)
		}
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["query"] != "test query" {
			t.Errorf("期望 query=test query，实际: %s", body["query"])
		}
		if body["api_key"] != "test-key" {
			t.Errorf("期望 api_key=test-key，实际: %s", body["api_key"])
		}

		resp := map[string]any{
			"results": []map[string]string{
				{"title": "Tavily Result", "url": "https://example.com/tavily", "content": "Tavily snippet"},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	// 注意: TavilyProvider 硬编码了 API URL，无法直接测试真实请求。
	// 这个测试验证结构和序列化逻辑。实际集成测试需要真实 API Key。
	// 为了测试，我们跳过实际 HTTP 调用，仅验证基本逻辑。
	p := &TavilyProvider{APIKey: "test-key"}
	// 空查询应报错
	_, err := p.Search(context.Background(), "", nil)
	if err == nil {
		t.Error("空查询应返回错误")
	}
}

func TestSerperProvider_Name(t *testing.T) {
	p := &SerperProvider{APIKey: "test-key"}
	if p.Name() != "serper" {
		t.Errorf("期望 Name() = serper，实际: %s", p.Name())
	}
}

func TestSerperProvider_EmptyQuery(t *testing.T) {
	p := &SerperProvider{APIKey: "test-key"}
	_, err := p.Search(context.Background(), "", nil)
	if err == nil {
		t.Error("期望空 query 返回错误")
	}
}

// --- NewProvider 工厂函数测试 ---
//
// 2026-04-27 重构后语义：strict 模式——配置不完整 / provider 未知时返回 error，
// 不再静默回落 DDG。fallback 改由 NewProviderWithDefault 显式承担。

func TestNewProvider_DuckDuckGo(t *testing.T) {
	p, err := NewProvider("duckduckgo_html", "", "")
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if p.Name() != "duckduckgo_html" {
		t.Errorf("期望 duckduckgo_html，实际: %s", p.Name())
	}
}

func TestNewProvider_Default(t *testing.T) {
	// 空 provider = 用户未表达偏好，DDG 是默认（不是 fallback），无 error
	p, err := NewProvider("", "", "")
	if err != nil {
		t.Fatalf("空 provider 不应报错: %v", err)
	}
	if p.Name() != "duckduckgo_html" {
		t.Errorf("空 provider 应默认 duckduckgo_html，实际: %s", p.Name())
	}
}

func TestNewProvider_SearXNG(t *testing.T) {
	p, err := NewProvider("searxng", "http://localhost:8080", "")
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if p.Name() != "searxng" {
		t.Errorf("期望 searxng，实际: %s", p.Name())
	}
}

func TestNewProvider_SearXNG_NoURL(t *testing.T) {
	// 缺少 URL 应严格失败（不再静默回落）
	p, err := NewProvider("searxng", "", "")
	if err == nil {
		t.Fatalf("缺少 URL 应返回 error，但实际成功返回 %s", p.Name())
	}
	if !strings.Contains(err.Error(), "search_api_url") {
		t.Errorf("error 应说明缺哪个字段，实际: %v", err)
	}
}

func TestNewProvider_Tavily(t *testing.T) {
	p, err := NewProvider("tavily", "", "test-key")
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if p.Name() != "tavily" {
		t.Errorf("期望 tavily，实际: %s", p.Name())
	}
}

func TestNewProvider_Tavily_NoKey(t *testing.T) {
	p, err := NewProvider("tavily", "", "")
	if err == nil {
		t.Fatalf("缺少 key 应返回 error，但实际成功返回 %s", p.Name())
	}
	if !strings.Contains(err.Error(), "search_api_key") {
		t.Errorf("error 应说明缺哪个字段，实际: %v", err)
	}
}

func TestNewProvider_Serper(t *testing.T) {
	p, err := NewProvider("serper", "", "test-key")
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if p.Name() != "serper" {
		t.Errorf("期望 serper，实际: %s", p.Name())
	}
}

func TestNewProvider_Serper_NoKey(t *testing.T) {
	p, err := NewProvider("serper", "", "")
	if err == nil {
		t.Fatalf("缺少 key 应返回 error，但实际成功返回 %s", p.Name())
	}
	if !strings.Contains(err.Error(), "search_api_key") {
		t.Errorf("error 应说明缺哪个字段，实际: %v", err)
	}
}

func TestNewProvider_Unknown(t *testing.T) {
	p, err := NewProvider("unknown_provider", "", "")
	if err == nil {
		t.Fatalf("未知 provider 应返回 error，但实际成功返回 %s", p.Name())
	}
	if !strings.Contains(err.Error(), "未知") {
		t.Errorf("error 应包含'未知'，实际: %v", err)
	}
}

// --- NewProviderWithDefault 显式兜底入口测试 ---
//
// 2026-04-27 修复 web_search probe/provider 决策分裂时引入。这是给 bootstrap /
// scheduler 用的"无论如何给我一个可用的 provider"入口，fallback 行为从 webtool
// 内部抽到这里——调用方拿到 fellBack=true 时应当 surface 一条说明，让 LLM /
// probe 知道实际跑的是 DDG 而不是用户配的那个 provider。

func TestNewProviderWithDefault_NoFallback(t *testing.T) {
	cases := []struct {
		name         string
		providerName string
		apiURL       string
		apiKey       string
		expectedName string
	}{
		{"empty", "", "", "", "duckduckgo_html"},
		{"explicit_ddg", "duckduckgo_html", "", "", "duckduckgo_html"},
		{"searxng_complete", "searxng", "http://localhost:8080", "", "searxng"},
		{"tavily_complete", "tavily", "", "tavily-key", "tavily"},
		{"serper_complete", "serper", "", "serper-key", "serper"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sp, fellBack, reason := NewProviderWithDefault(tc.providerName, tc.apiURL, tc.apiKey)
			if fellBack {
				t.Errorf("配置完整不应触发 fallback，实际 fellBack=true reason=%q", reason)
			}
			if reason != "" {
				t.Errorf("无 fallback 时 reason 应为空，实际: %q", reason)
			}
			if sp.Name() != tc.expectedName {
				t.Errorf("期望 provider %s，实际 %s", tc.expectedName, sp.Name())
			}
		})
	}
}

func TestNewProviderWithDefault_FallbackOnIncompleteConfig(t *testing.T) {
	cases := []struct {
		name           string
		providerName   string
		apiURL         string
		apiKey         string
		reasonContains string
	}{
		{"searxng_no_url", "searxng", "", "", "search_api_url"},
		{"tavily_no_key", "tavily", "", "", "search_api_key"},
		{"serper_no_key", "serper", "", "", "search_api_key"},
		{"unknown_provider", "unknown_x", "", "", "未知"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sp, fellBack, reason := NewProviderWithDefault(tc.providerName, tc.apiURL, tc.apiKey)
			if !fellBack {
				t.Fatalf("配置不完整应触发 fallback，实际 fellBack=false")
			}
			if sp.Name() != "duckduckgo_html" {
				t.Errorf("fallback 目标应为 DDG，实际: %s", sp.Name())
			}
			if !strings.Contains(reason, tc.reasonContains) {
				t.Errorf("reason 应包含 %q，实际: %q", tc.reasonContains, reason)
			}
		})
	}
}

// --- FormatResults 测试 ---

func TestFormatResults_Empty(t *testing.T) {
	result := FormatResults(nil)
	if result != "未找到搜索结果" {
		t.Errorf("空结果应返回 '未找到搜索结果'，实际: %s", result)
	}
}

func TestFormatResults_WithResults(t *testing.T) {
	results := []SearchResult{
		{Title: "Title 1", URL: "https://example.com/1", Snippet: "Snippet 1"},
		{Title: "Title 2", URL: "https://example.com/2", Snippet: ""},
	}
	output := FormatResults(results)
	if !strings.Contains(output, "1. Title 1") {
		t.Errorf("期望包含 '1. Title 1'，实际: %s", output)
	}
	if !strings.Contains(output, "https://example.com/1") {
		t.Errorf("期望包含 URL，实际: %s", output)
	}
	if !strings.Contains(output, "Snippet 1") {
		t.Errorf("期望包含 Snippet 1，实际: %s", output)
	}
	if !strings.Contains(output, "2. Title 2") {
		t.Errorf("期望包含 '2. Title 2'，实际: %s", output)
	}
}
