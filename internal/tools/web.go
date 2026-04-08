package tools

import (
	"context"
	"fmt"

	"agentgo/internal/agent"
	"agentgo/internal/tools/schema"
	"agentgo/internal/webtool"
)

// WebGroup 注册网络检索相关的工具：web_search 和 web_fetch。
// 当 Provider 为 nil 时，Register 不会注册任何工具（静默跳过），
// 调用方可以无条件地把 WebGroup{} 加进 RegisterGroups 列表。
type WebGroup struct {
	Provider webtool.SearchProvider
}

// maxWebSearchResults 限制单次 web_search 的最大返回条数。
const maxWebSearchResults = 10

// Register 实现 ToolGroup 接口。
func (g WebGroup) Register(r *agent.ToolRegistry) {
	if g.Provider == nil {
		return
	}

	searchParams := schema.Object().
		String("query", "搜索关键词", true).
		Int("max_results", "返回结果数量，默认 5，最大 10", false).
		Enum("time_range", "时间范围过滤，默认 any。调查近期事件时建议设为 week 或 month",
			[]string{"any", "day", "week", "month", "year"}, false).
		Build()

	r.Register("web_search", "使用搜索引擎搜索网络信息", searchParams,
		func(ctx context.Context, args map[string]any) (string, error) {
			query, _ := args["query"].(string)
			if query == "" {
				return "", fmt.Errorf("缺少 query 参数")
			}
			opts := &webtool.SearchOptions{}
			if n, ok := args["max_results"].(float64); ok && n > 0 {
				opts.NumResults = int(n)
				if opts.NumResults > maxWebSearchResults {
					opts.NumResults = maxWebSearchResults
				}
			}
			if tr, ok := args["time_range"].(string); ok && tr != "" {
				opts.TimeRange = tr
			}
			results, err := g.Provider.Search(ctx, query, opts)
			if err != nil {
				return "", err
			}
			return webtool.FormatResults(results), nil
		})

	fetchParams := schema.Object().
		String("url", "要获取的网页 URL", true).
		Enum("extract_mode", "内容提取模式：auto=智能判断；article=只提取正文（过滤导航/页脚噪音）；full=全页面文本。默认 auto",
			[]string{"auto", "article", "full"}, false).
		Build()

	r.Register("web_fetch", "获取指定 URL 的网页文本内容", fetchParams,
		func(ctx context.Context, args map[string]any) (string, error) {
			rawURL, _ := args["url"].(string)
			mode, _ := args["extract_mode"].(string)
			if mode == "" {
				mode = "auto"
			}
			return webtool.FetchURLWithMode(ctx, rawURL, mode)
		})
}
