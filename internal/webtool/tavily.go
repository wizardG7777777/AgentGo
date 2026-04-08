package webtool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// TavilyProvider 通过 Tavily API 实现搜索。
// 需要配置 APIKey。
type TavilyProvider struct {
	APIKey string
}

func (t *TavilyProvider) Name() string { return "tavily" }

func (t *TavilyProvider) Search(ctx context.Context, query string, opts *SearchOptions) ([]SearchResult, error) {
	if query == "" {
		return nil, fmt.Errorf("缺少 query 参数")
	}

	// Tavily API: POST https://api.tavily.com/search
	payload := map[string]any{
		"query":       query,
		"api_key":     t.APIKey,
		"max_results": 5,
	}
	if opts != nil {
		if opts.NumResults > 0 {
			payload["max_results"] = opts.NumResults
		}
		// Tavily 使用 days 参数：day=1, week=7, month=30, year=365
		if opts.TimeRange != "" && opts.TimeRange != "any" {
			days := map[string]int{"day": 1, "week": 7, "month": 30, "year": 365}
			if d, ok := days[opts.TimeRange]; ok {
				payload["days"] = d
			}
		}
	}
	reqBody, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("构建 Tavily 请求体失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.tavily.com/search", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("创建 Tavily 请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)

	client := &http.Client{Timeout: defaultTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Tavily 请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Tavily 返回 HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("读取 Tavily 响应失败: %w", err)
	}

	// 解析 Tavily JSON 响应
	var apiResp struct {
		Results []struct {
			Title         string  `json:"title"`
			URL           string  `json:"url"`
			Content       string  `json:"content"`
			PublishedDate string  `json:"published_date"`
			Score         float64 `json:"score"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("解析 Tavily 响应失败: %w", err)
	}

	var results []SearchResult
	for _, r := range apiResp.Results {
		// 从 URL 提取域名作为 Source
		source := extractDomain(r.URL)
		results = append(results, SearchResult{
			Title:       r.Title,
			URL:         r.URL,
			Snippet:     r.Content,
			PublishedAt: r.PublishedDate,
			Source:      source,
			Score:       r.Score,
		})
	}
	return results, nil
}
