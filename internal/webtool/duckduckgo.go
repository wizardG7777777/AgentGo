package webtool

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// DuckDuckGoProvider 通过抓取 DuckDuckGo HTML 页面实现搜索。
// 无需 API Key，但可能受到频率限制。
type DuckDuckGoProvider struct{}

func (d *DuckDuckGoProvider) Name() string { return "duckduckgo_html" }

func (d *DuckDuckGoProvider) Search(ctx context.Context, query string) ([]SearchResult, error) {
	if query == "" {
		return nil, fmt.Errorf("缺少 query 参数")
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
	return results, nil
}
