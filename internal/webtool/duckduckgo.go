package webtool

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
)

// DuckDuckGoProvider 通过抓取 DuckDuckGo HTML 页面实现搜索。
// 无需 API Key，但可能受到频率限制。
type DuckDuckGoProvider struct{}

func (d *DuckDuckGoProvider) Name() string { return "duckduckgo_html" }

func (d *DuckDuckGoProvider) Search(ctx context.Context, query string, opts *SearchOptions) ([]SearchResult, error) {
	if query == "" {
		return nil, fmt.Errorf("缺少 query 参数")
	}
	// DuckDuckGo HTML 模式不支持 time_range 过滤；NumResults 通过截断结果列表实现
	numResults := 5
	if opts != nil && opts.NumResults > 0 {
		numResults = opts.NumResults
	}
	if opts != nil && opts.TimeRange != "" && opts.TimeRange != "any" {
		log.Printf("[webtool] duckduckgo_html 不支持 time_range=%q 过滤，忽略", opts.TimeRange)
	}

	searchURL := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(query)

	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建搜索请求失败: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)

	client := &http.Client{Timeout: defaultTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("搜索请求失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("读取搜索结果失败: %w", err)
	}

	results := ParseSearchResults(string(body))
	if len(results) > numResults {
		results = results[:numResults]
	}
	return results, nil
}
