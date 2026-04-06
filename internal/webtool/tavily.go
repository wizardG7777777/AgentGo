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

func (t *TavilyProvider) Search(ctx context.Context, query string) ([]SearchResult, error) {
	if query == "" {
		return nil, fmt.Errorf("缺少 query 参数")
	}

	// Tavily API: POST https://api.tavily.com/search
	reqBody, err := json.Marshal(map[string]string{
		"query":   query,
		"api_key": t.APIKey,
	})
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
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("解析 Tavily 响应失败: %w", err)
	}

	var results []SearchResult
	for _, r := range apiResp.Results {
		results = append(results, SearchResult{
			Title:   r.Title,
			URL:     r.URL,
			Snippet: r.Content,
		})
	}
	return results, nil
}
