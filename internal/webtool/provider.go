package webtool

import (
	"context"
	"fmt"
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

// NewProvider 严格按 provider 名 + 配置完整性构造 SearchProvider。
//
// 行为约定（2026-04-27 重构，历史设计见 docs/archived/nextUpgrade_v5.md §11.1 案例块）：
//   - 空字符串 / "duckduckgo_html" → DuckDuckGoProvider, nil
//   - "searxng" 缺 apiURL / "tavily"·"serper" 缺 apiKey → nil, error
//   - 未知 provider → nil, error（不再静默回落 DDG，避免 probe/provider 决策分裂）
//
// 不再做静默 fallback——历史问题：probe 与 provider 各自看同一份配置（serper +
// 空 key）得出矛盾结论（probe 报 unavailable / provider 静默切 DDG），LLM 误以为
// web_search 不可用而拒绝调用。fallback 决策从 webtool 抽到上层（bootstrap），
// 由 NewProviderWithDefault 统一兜底并显式告知调用方。
//
// 调用方需要"无论如何给我一个可用的 provider"时改用 NewProviderWithDefault。
func NewProvider(provider, apiURL, apiKey string) (SearchProvider, error) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "", "duckduckgo_html":
		return &DuckDuckGoProvider{}, nil
	case "searxng":
		if apiURL == "" {
			return nil, fmt.Errorf("searxng 提供者需要 search_api_url")
		}
		return &SearXNGProvider{BaseURL: strings.TrimRight(apiURL, "/")}, nil
	case "tavily":
		if apiKey == "" {
			return nil, fmt.Errorf("tavily 提供者需要 search_api_key")
		}
		return &TavilyProvider{APIKey: apiKey}, nil
	case "serper":
		if apiKey == "" {
			return nil, fmt.Errorf("serper 提供者需要 search_api_key")
		}
		return &SerperProvider{APIKey: apiKey}, nil
	default:
		return nil, fmt.Errorf("未知的搜索提供者: %s", provider)
	}
}

// NewProviderWithDefault 是带兜底的 provider 构造入口：strict NewProvider 失败
// 时回落 DuckDuckGo，并通过 fellBack/reason 显式告知调用方"用户要的不是实际跑的"。
//
//   - sp:        实际运行的 SearchProvider（永不返回 nil）
//   - fellBack:  true 表示触发了兜底（调用方应当 surface 一条说明，便于排障）
//   - reason:    人类可读的兜底原因（fellBack=false 时为空串）
//
// 这是 bootstrap / scheduler 推荐入口；要严格校验配置时直接用 NewProvider。
func NewProviderWithDefault(provider, apiURL, apiKey string) (sp SearchProvider, fellBack bool, reason string) {
	sp, err := NewProvider(provider, apiURL, apiKey)
	if err == nil {
		return sp, false, ""
	}
	return &DuckDuckGoProvider{}, true, err.Error()
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
