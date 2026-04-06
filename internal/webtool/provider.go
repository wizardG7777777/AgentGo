package webtool

import (
	"context"
	"fmt"
	"log"
	"strings"
)

// SearchProvider 搜索引擎提供者接口，支持可插拔的搜索后端。
type SearchProvider interface {
	// Search 执行搜索查询，返回结构化搜索结果。
	Search(ctx context.Context, query string) ([]SearchResult, error)
	// Name 返回提供者的标识名称（如 "duckduckgo_html"、"searxng"）。
	Name() string
}

// NewProvider 根据配置创建对应的 SearchProvider 实例。
// provider 为空时默认使用 DuckDuckGo HTML 抓取方式。
func NewProvider(provider, apiURL, apiKey string) SearchProvider {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "searxng":
		if apiURL == "" {
			log.Println("[警告] searxng 提供者需要 search_api_url，回退到 duckduckgo_html")
			return &DuckDuckGoProvider{}
		}
		return &SearXNGProvider{BaseURL: strings.TrimRight(apiURL, "/")}
	case "tavily":
		if apiKey == "" {
			log.Println("[警告] tavily 提供者需要 search_api_key，回退到 duckduckgo_html")
			return &DuckDuckGoProvider{}
		}
		return &TavilyProvider{APIKey: apiKey}
	case "serper":
		if apiKey == "" {
			log.Println("[警告] serper 提供者需要 search_api_key，回退到 duckduckgo_html")
			return &DuckDuckGoProvider{}
		}
		return &SerperProvider{APIKey: apiKey}
	case "duckduckgo_html", "":
		if provider == "" {
			log.Println("[提示] 未配置 search_api_provider，使用默认 duckduckgo_html")
		}
		return &DuckDuckGoProvider{}
	default:
		log.Printf("[警告] 未知的搜索提供者 %q，回退到 duckduckgo_html\n", provider)
		return &DuckDuckGoProvider{}
	}
}

// FormatResults 将搜索结果列表格式化为可读的文本输出，供 LLM 消费。
func FormatResults(results []SearchResult) string {
	if len(results) == 0 {
		return "未找到搜索结果"
	}
	var sb strings.Builder
	for i, r := range results {
		sb.WriteString(fmt.Sprintf("%d. %s\n   %s\n", i+1, r.Title, r.URL))
		if r.Snippet != "" {
			sb.WriteString(fmt.Sprintf("   %s\n", r.Snippet))
		}
	}
	return sb.String()
}
