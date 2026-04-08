package webtool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// SearXNGProvider 通过 SearXNG 实例的 JSON API 实现搜索。
// 需要配置 BaseURL 指向 SearXNG 实例地址。
type SearXNGProvider struct {
	BaseURL string
}

func (s *SearXNGProvider) Name() string { return "searxng" }

func (s *SearXNGProvider) Search(ctx context.Context, query string, opts *SearchOptions) ([]SearchResult, error) {
	if query == "" {
		return nil, fmt.Errorf("缺少 query 参数")
	}

	// SearXNG JSON API: GET {baseURL}/search?q={query}&format=json[&time_range=day|week|month|year]
	apiURL := fmt.Sprintf("%s/search?q=%s&format=json", s.BaseURL, url.QueryEscape(query))
	if opts != nil && opts.TimeRange != "" && opts.TimeRange != "any" {
		apiURL += "&time_range=" + url.QueryEscape(opts.TimeRange)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建 SearXNG 请求失败: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: defaultTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("SearXNG 请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("SearXNG 返回 HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("读取 SearXNG 响应失败: %w", err)
	}

	// 解析 SearXNG JSON 响应
	var apiResp struct {
		Results []struct {
			Title         string  `json:"title"`
			URL           string  `json:"url"`
			Content       string  `json:"content"`
			PublishedDate string  `json:"publishedDate"`
			Engine        string  `json:"engine"`
			Score         float64 `json:"score"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("解析 SearXNG 响应失败: %w", err)
	}

	numResults := 5
	if opts != nil && opts.NumResults > 0 {
		numResults = opts.NumResults
	}

	var results []SearchResult
	for _, r := range apiResp.Results {
		if len(results) >= numResults {
			break
		}
		results = append(results, SearchResult{
			Title:       r.Title,
			URL:         r.URL,
			Snippet:     r.Content,
			PublishedAt: r.PublishedDate,
			Source:      r.Engine,
			Score:       r.Score,
		})
	}
	return results, nil
}
