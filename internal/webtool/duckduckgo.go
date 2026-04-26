package webtool

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
)

// DuckDuckGoProvider 通过抓取 DuckDuckGo HTML 页面实现搜索。
// 无需 API Key，但可能受到频率限制。
type DuckDuckGoProvider struct{}

// unwrapDDGRedirect 把 DuckDuckGo HTML 搜索结果里的中转链接还原为真实 URL。
//
// DDG HTML 把所有外链包成 `//duckduckgo.com/l/?uddg=<encoded_real_url>&rut=...`
// （反爬 + 跟踪），原样回传给 LLM 会出三种问题：
//  1. 以 `//` 开头无 scheme，net/url 解析失败
//  2. 即使补 https，请求的是 DDG 中转，多一跳且容易被反爬拦截
//  3. "来源 = duckduckgo.com" 而非真实站点，LLM 看到的来源可信度被稀释
//
// 2026-04-27 网络冒烟测试发现的 bug——之前 web_search 返回的都是中转 URL，
// 但因为 LLM 端拿到结果后通常不再 web_fetch，这层 bug 一直没暴露。修复时机：
// 修 fallback 决策分裂之后，顺手把 DDG provider 的输出质量也补齐。
//
// 行为：识别到 DDG /l/ 路径时返回 uddg 参数解出的真 URL；其他情况原样返回。
func unwrapDDGRedirect(href string) string {
	// 1) 还原 HTML 实体编码：href 里的 & 会被写成 &amp;，url.Parse 不认
	href = strings.ReplaceAll(href, "&amp;", "&")
	// 2) 补 scheme：DDG 经常用 //duckduckgo.com/... 这种 protocol-relative URL
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	u, err := url.Parse(href)
	if err != nil {
		return href // best effort——解析失败就原样返回
	}
	host := strings.ToLower(u.Host)
	if (host == "duckduckgo.com" || host == "www.duckduckgo.com") && u.Path == "/l/" {
		if real := u.Query().Get("uddg"); real != "" {
			// uddg 参数本身已被 query 层 URL-decode 一次，可直接使用
			return real
		}
	}
	return href
}

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
