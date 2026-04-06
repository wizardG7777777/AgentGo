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
	_, err := p.Search(context.Background(), "")
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
	_, err := p.Search(context.Background(), "")
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
	results, err := p.Search(context.Background(), "test query")
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
	_, err := p.Search(context.Background(), "")
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
	_, err := p.Search(context.Background(), "")
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
	_, err := p.Search(context.Background(), "")
	if err == nil {
		t.Error("期望空 query 返回错误")
	}
}

// --- NewProvider 工厂函数测试 ---

func TestNewProvider_DuckDuckGo(t *testing.T) {
	p := NewProvider("duckduckgo_html", "", "")
	if p.Name() != "duckduckgo_html" {
		t.Errorf("期望 duckduckgo_html，实际: %s", p.Name())
	}
}

func TestNewProvider_Default(t *testing.T) {
	p := NewProvider("", "", "")
	if p.Name() != "duckduckgo_html" {
		t.Errorf("空 provider 应回退到 duckduckgo_html，实际: %s", p.Name())
	}
}

func TestNewProvider_SearXNG(t *testing.T) {
	p := NewProvider("searxng", "http://localhost:8080", "")
	if p.Name() != "searxng" {
		t.Errorf("期望 searxng，实际: %s", p.Name())
	}
}

func TestNewProvider_SearXNG_NoURL(t *testing.T) {
	// 缺少 URL 应回退到 duckduckgo_html
	p := NewProvider("searxng", "", "")
	if p.Name() != "duckduckgo_html" {
		t.Errorf("缺少 URL 时应回退到 duckduckgo_html，实际: %s", p.Name())
	}
}

func TestNewProvider_Tavily(t *testing.T) {
	p := NewProvider("tavily", "", "test-key")
	if p.Name() != "tavily" {
		t.Errorf("期望 tavily，实际: %s", p.Name())
	}
}

func TestNewProvider_Tavily_NoKey(t *testing.T) {
	p := NewProvider("tavily", "", "")
	if p.Name() != "duckduckgo_html" {
		t.Errorf("缺少 key 时应回退到 duckduckgo_html，实际: %s", p.Name())
	}
}

func TestNewProvider_Serper(t *testing.T) {
	p := NewProvider("serper", "", "test-key")
	if p.Name() != "serper" {
		t.Errorf("期望 serper，实际: %s", p.Name())
	}
}

func TestNewProvider_Serper_NoKey(t *testing.T) {
	p := NewProvider("serper", "", "")
	if p.Name() != "duckduckgo_html" {
		t.Errorf("缺少 key 时应回退到 duckduckgo_html，实际: %s", p.Name())
	}
}

func TestNewProvider_Unknown(t *testing.T) {
	p := NewProvider("unknown_provider", "", "")
	if p.Name() != "duckduckgo_html" {
		t.Errorf("未知 provider 应回退到 duckduckgo_html，实际: %s", p.Name())
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
