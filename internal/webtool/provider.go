package webtool

import (
	"context"
	"fmt"
	"log"
	"strings"
)

// SearchOptions 控制搜索行为的可选参数。传 nil 时各后端使用自身默认值。
type SearchOptions struct {
	NumResults int            // 返回结果数，0 表示使用后端默认值（通常为 5）
	TimeRange  string         // "any" | "day" | "week" | "month" | "year"，空串表示 "any"
	Extra      map[string]any // 后端特定扩展参数，向前兼容
}

// SearchProvider 搜索引擎提供者接口，支持可插拔的搜索后端。
type SearchProvider interface {
	// Search 执行搜索查询，返回结构化搜索结果。opts 为 nil 时使用各后端默认值。
	Search(ctx context.Context, query string, opts *SearchOptions) ([]SearchResult, error)
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
// 当 SearchResult 包含 PublishedAt 或 Source 时，会在 URL 行附加元数据。
func FormatResults(results []SearchResult) string {
	if len(results) == 0 {
		return "未找到搜索结果"
	}
	var sb strings.Builder
	for i, r := range results {
		sb.WriteString(fmt.Sprintf("%d. %s\n   %s\n", i+1, r.Title, r.URL))
		// 元数据行：来源域名 + 发布时间（有一个就显示）
		if r.Source != "" || r.PublishedAt != "" {
			var meta []string
			if r.Source != "" {
				meta = append(meta, "来源: "+r.Source)
			}
			if r.PublishedAt != "" {
				meta = append(meta, "发布: "+r.PublishedAt)
			}
			sb.WriteString("   [" + strings.Join(meta, " | ") + "]\n")
		}
		if r.Snippet != "" {
			sb.WriteString(fmt.Sprintf("   %s\n", r.Snippet))
		}
	}
	return sb.String()
}
