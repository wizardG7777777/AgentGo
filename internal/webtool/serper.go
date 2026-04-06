package webtool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// SerperProvider 通过 Serper（Google Search API）实现搜索。
// 需要配置 APIKey。
type SerperProvider struct {
	APIKey string
}

func (s *SerperProvider) Name() string { return "serper" }

func (s *SerperProvider) Search(ctx context.Context, query string) ([]SearchResult, error) {
	if query == "" {
		return nil, fmt.Errorf("缺少 query 参数")
	}

	// Serper API: POST https://google.serper.dev/search
	reqBody, err := json.Marshal(map[string]string{
		"q": query,
	})
	if err != nil {
		return nil, fmt.Errorf("构建 Serper 请求体失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://google.serper.dev/search", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("创建 Serper 请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-KEY", s.APIKey)
	req.Header.Set("User-Agent", userAgent)

	client := &http.Client{Timeout: defaultTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Serper 请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Serper 返回 HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("读取 Serper 响应失败: %w", err)
	}

	// 解析 Serper JSON 响应（organic 字段包含搜索结果）
	var apiResp struct {
		Organic []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
		} `json:"organic"`
	}
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("解析 Serper 响应失败: %w", err)
	}

	var results []SearchResult
	for _, r := range apiResp.Organic {
		results = append(results, SearchResult{
			Title:   r.Title,
			URL:     r.Link,
			Snippet: r.Snippet,
		})
	}
	return results, nil
}
