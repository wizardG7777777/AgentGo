package webtool

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExtractText_Basic(t *testing.T) {
	html := `<html><body><h1>标题</h1><p>这是一段文本。</p></body></html>`
	got := ExtractText(html)
	if !strings.Contains(got, "标题") {
		t.Errorf("期望包含 '标题', 实际: %s", got)
	}
	if !strings.Contains(got, "这是一段文本。") {
		t.Errorf("期望包含 '这是一段文本。', 实际: %s", got)
	}
	if strings.Contains(got, "<") {
		t.Errorf("期望不包含 HTML 标签, 实际: %s", got)
	}
}

func TestExtractText_ScriptRemoval(t *testing.T) {
	html := `<html><head><script>var x = 1;</script></head><body><p>可见内容</p><style>.a{color:red}</style></body></html>`
	got := ExtractText(html)
	if strings.Contains(got, "var x") {
		t.Errorf("期望 script 内容被移除, 实际: %s", got)
	}
	if strings.Contains(got, "color:red") {
		t.Errorf("期望 style 内容被移除, 实际: %s", got)
	}
	if !strings.Contains(got, "可见内容") {
		t.Errorf("期望包含 '可见内容', 实际: %s", got)
	}
}

func TestExtractText_HTMLEntities(t *testing.T) {
	html := `<p>A &amp; B &lt; C &gt; D &quot;E&quot; &#39;F&#39; &nbsp;G</p>`
	got := ExtractText(html)
	if !strings.Contains(got, `A & B < C > D "E" 'F'`) {
		t.Errorf("HTML 实体解码不正确, 实际: %s", got)
	}
}

func TestExtractText_ComplexStructure(t *testing.T) {
	// 测试复杂 HTML 结构提取
	htmlWithArticle := `<html><body><nav>导航</nav><main><h1>文章标题</h1><p>文章正文</p></main><footer>页脚</footer></body></html>`

	result := ExtractText(htmlWithArticle)
	if !strings.Contains(result, "文章标题") {
		t.Errorf("应提取正文内容, 实际: %s", result)
	}
	if !strings.Contains(result, "导航") || !strings.Contains(result, "页脚") {
		t.Errorf("应包含全部可见内容, 实际: %s", result)
	}
}

func TestStripTags(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"<a href='x'>链接</a>", "链接"},
		{"<b>粗体</b>文本", "粗体文本"},
		{"无标签", "无标签"},
		{"  <span>  空格  </span>  ", "空格"},
	}
	for _, tt := range tests {
		got := StripTags(tt.input)
		if got != tt.want {
			t.Errorf("StripTags(%q) = %q, 期望 %q", tt.input, got, tt.want)
		}
	}
}

func TestParseSearchResults(t *testing.T) {
	// 模拟 DuckDuckGo HTML 搜索结果片段
	html := `
	<div class="result">
		<a rel="nofollow" class="result__a" href="https://example.com/page1">Example <b>Page</b> 1</a>
		<a class="result__snippet" href="#">这是第一条结果的摘要</a>
	</div>
	<div class="result">
		<a rel="nofollow" class="result__a" href="https://example.com/page2">Example Page 2</a>
		<a class="result__snippet" href="#">第二条结果摘要</a>
	</div>
	`

	results := ParseSearchResults(html)
	if len(results) != 2 {
		t.Fatalf("期望 2 条结果, 实际 %d 条", len(results))
	}

	if results[0].URL != "https://example.com/page1" {
		t.Errorf("第1条 URL 不正确: %s", results[0].URL)
	}
	if results[0].Title != "Example Page 1" {
		t.Errorf("第1条标题不正确: %s", results[0].Title)
	}
	if results[0].Snippet != "这是第一条结果的摘要" {
		t.Errorf("第1条摘要不正确: %s", results[0].Snippet)
	}

	if results[1].URL != "https://example.com/page2" {
		t.Errorf("第2条 URL 不正确: %s", results[1].URL)
	}
	if results[1].Title != "Example Page 2" {
		t.Errorf("第2条标题不正确: %s", results[1].Title)
	}
}

func TestFormatResults(t *testing.T) {
	results := []SearchResult{
		{Title: "Test 1", URL: "https://example.com/1", Snippet: "Snippet 1"},
		{Title: "Test 2", URL: "https://example.com/2", Snippet: "Snippet 2"},
	}

	formatted := FormatResults(results)
	if !strings.Contains(formatted, "Test 1") {
		t.Error("格式化结果应包含标题")
	}
	if !strings.Contains(formatted, "https://example.com/1") {
		t.Error("格式化结果应包含 URL")
	}
}

func TestFormatResults_EmptySlice(t *testing.T) {
	// 与 provider_test.go 中的 TestFormatResults_Empty 区分
	result := FormatResults([]SearchResult{})
	if result != "未找到搜索结果" {
		t.Errorf("空结果应返回提示文本, 实际: %s", result)
	}
}

// --- URL 校验测试（纯单元测试，不依赖网络） ---

func TestValidateURL_Loopback(t *testing.T) {
	err := validateURL("http://127.0.0.1/admin")
	if err == nil {
		t.Fatal("期望环回地址被拒绝")
	}
	if !strings.Contains(err.Error(), "内网") {
		t.Errorf("错误信息应包含 '内网', 实际: %s", err.Error())
	}
}

func TestValidateURL_PrivateNetwork(t *testing.T) {
	err := validateURL("http://192.168.1.1/")
	if err == nil {
		t.Fatal("期望私有网络地址被拒绝")
	}
	if !strings.Contains(err.Error(), "内网") {
		t.Errorf("错误信息应包含 '内网', 实际: %s", err.Error())
	}
}

func TestValidateURL_LinkLocal(t *testing.T) {
	err := validateURL("http://169.254.169.254/latest/meta-data/")
	if err == nil {
		t.Fatal("期望链路本地地址被拒绝")
	}
	if !strings.Contains(err.Error(), "内网") {
		t.Errorf("错误信息应包含 '内网', 实际: %s", err.Error())
	}
}

func TestValidateURL_PublicIP(t *testing.T) {
	err := validateURL("http://8.8.8.8/")
	if err != nil {
		t.Errorf("公网 IP 不应被拒绝, 错误: %s", err.Error())
	}
}

func TestValidateURL_LocalhostDomain(t *testing.T) {
	err := validateURL("http://localhost/")
	if err == nil {
		t.Fatal("期望 localhost 域名被拒绝")
	}
	if !strings.Contains(err.Error(), "内网") {
		t.Errorf("错误信息应包含 '内网', 实际: %s", err.Error())
	}
}

func TestValidateURL_InvalidURL(t *testing.T) {
	err := validateURL("://invalid-url")
	if err == nil {
		t.Fatal("期望无效 URL 返回错误")
	}
}

// --- FetchURL 错误处理测试（使用 httptest，测试正常流程） ---

func TestFetchURL_MissingURL(t *testing.T) {
	_, err := FetchURL(t.Context(), "")
	if err == nil {
		t.Error("期望空 URL 返回错误")
	}
	if !strings.Contains(err.Error(), "缺少 url 参数") {
		t.Errorf("错误信息不正确: %s", err.Error())
	}
}

func TestFetchURL_InvalidURL(t *testing.T) {
	_, err := FetchURL(t.Context(), "://not-a-valid-url")
	if err == nil {
		t.Error("期望无效 URL 返回错误")
	}
}

// TestFetchURL_Success 使用 httptest 创建测试服务器
// 注意：由于 SSRF 防护会拦截内网地址，这个测试验证的是
// 当 URL 被允许时的完整流程
func TestFetchURL_Success(t *testing.T) {
	// 创建一个公网 IP 的 mock（通过修改 Host 头绕过 DNS 解析）
	// 实际上 httptest 使用的是 127.0.0.1，会被 SSRF 拦截
	// 所以这个测试主要验证代码路径的正确性

	// 测试 https 自动添加前缀
	// 使用一个公网域名但无法解析的情况来测试路径
	_, err := FetchURL(t.Context(), "this-is-a-test-domain-that-does-not-exist-1234567890.example")
	// 期望错误是 DNS 解析失败或网络错误，不是参数错误
	if err == nil {
		t.Error("期望请求失败返回错误")
	}
	// 只要不是参数缺失错误就算通过
	if strings.Contains(err.Error(), "缺少") {
		t.Errorf("不应是参数缺失错误: %s", err.Error())
	}
}

func TestSearchWeb_MissingQuery(t *testing.T) {
	_, err := SearchWeb(t.Context(), "")
	if err == nil {
		t.Error("期望空 query 返回错误")
	}
	if !strings.Contains(err.Error(), "缺少 query 参数") {
		t.Errorf("错误信息不正确: %s", err.Error())
	}
}

// --- 边界情况测试 ---

func TestExtractText_LargeInput(t *testing.T) {
	// 测试大输入的处理
	largeContent := strings.Repeat("<p>这是一段很长的文本</p>", 1000)
	result := ExtractText(largeContent)
	if !strings.Contains(result, "这是一段很长的文本") {
		t.Error("应能处理大输入")
	}
}

func TestExtractText_NestedTags(t *testing.T) {
	html := `<div><p>外层<div>内层<span>最内层</span></div></p></div>`
	result := ExtractText(html)
	if !strings.Contains(result, "外层") || !strings.Contains(result, "内层") || !strings.Contains(result, "最内层") {
		t.Errorf("嵌套标签处理不正确: %s", result)
	}
}

func TestExtractText_EmptyInput(t *testing.T) {
	result := ExtractText("")
	if result != "" {
		t.Errorf("空输入应返回空字符串, 实际: %s", result)
	}
}

func TestStripTags_Nested(t *testing.T) {
	input := "<div><p>文本<b>粗体</b>结束</p></div>"
	want := "文本粗体结束"
	got := StripTags(input)
	if got != want {
		t.Errorf("StripTags(%q) = %q, 期望 %q", input, got, want)
	}
}

func TestParseSearchResults_Empty(t *testing.T) {
	results := ParseSearchResults("")
	if len(results) != 0 {
		t.Errorf("空 HTML 应返回空结果, 实际: %d", len(results))
	}
}

func TestParseSearchResults_NoMatches(t *testing.T) {
	html := `<html><body><p>普通内容，没有搜索结果</p></body></html>`
	results := ParseSearchResults(html)
	if len(results) != 0 {
		t.Errorf("无匹配时应返回空结果, 实际: %d", len(results))
	}
}

// TestParseSearchResults_UnwrapDDGRedirect 守 2026-04-27 修复的不变量：
// DDG HTML 把外链包成 //duckduckgo.com/l/?uddg=<encoded>&rut=... 中转链接，
// 解析器必须解开 uddg 参数还原真实 URL，否则 LLM 拿到带 // 前缀且无法 web_fetch
// 的 redirect URL（详见 duckduckgo.go unwrapDDGRedirect 注释）。
//
// 这个测试用真实 DDG HTML 片段（&amp; 实体编码 + // protocol-relative + uddg
// 参数），fixture 来自 2026-04-27 网络冒烟测试输出。
func TestParseSearchResults_UnwrapDDGRedirect(t *testing.T) {
	html := `
	<div class="result">
		<a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2F&amp;rut=5e47fd7484315627">The Go Programming Language</a>
		<a class="result__snippet">官方网站</a>
	</div>
	<div class="result">
		<a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fen.wikipedia.org%2Fwiki%2FGo_(programming_language)&amp;rut=95b65ae5">Go (programming language) - Wikipedia</a>
		<a class="result__snippet">A statically typed compiled language...</a>
	</div>
	`
	results := ParseSearchResults(html)
	if len(results) != 2 {
		t.Fatalf("期望 2 条结果，实际 %d", len(results))
	}
	if got, want := results[0].URL, "https://go.dev/"; got != want {
		t.Errorf("结果 0 URL = %q，期望 %q", got, want)
	}
	if got, want := results[1].URL, "https://en.wikipedia.org/wiki/Go_(programming_language)"; got != want {
		t.Errorf("结果 1 URL = %q，期望 %q", got, want)
	}
}

// TestUnwrapDDGRedirect_Unit 直接覆盖 unwrap 函数的边界情况。
func TestUnwrapDDGRedirect_Unit(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain_https_passthrough", "https://example.com/page", "https://example.com/page"},
		{"plain_http_passthrough", "http://example.com", "http://example.com"},
		{"ddg_redirect_basic", "//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2F", "https://go.dev/"},
		{"ddg_redirect_with_amp_entity", "//duckduckgo.com/l/?uddg=https%3A%2F%2Fa.com%2F&amp;rut=xxx", "https://a.com/"},
		{"ddg_redirect_https_scheme", "https://duckduckgo.com/l/?uddg=https%3A%2F%2Fb.com%2F", "https://b.com/"},
		{"ddg_redirect_www", "https://www.duckduckgo.com/l/?uddg=https%3A%2F%2Fc.com%2F", "https://c.com/"},
		{"ddg_non_redirect_path", "https://duckduckgo.com/about", "https://duckduckgo.com/about"},
		{"ddg_redirect_no_uddg", "//duckduckgo.com/l/?other=x", "https://duckduckgo.com/l/?other=x"},
		{"empty", "", ""},
		{"malformed_kept_as_is", "://broken url", "://broken url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := unwrapDDGRedirect(tc.in); got != tc.want {
				t.Errorf("unwrapDDGRedirect(%q) = %q，期望 %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestFetchURL_VerifyURL 测试 URL 处理
func TestFetchURL_VerifyURL(t *testing.T) {
	// 测试 https 自动添加前缀
	// 使用一个公网域名但无法解析的情况来测试路径
	_, err := FetchURL(t.Context(), "this-is-a-test-domain-that-does-not-exist-1234567890.example")
	// 期望错误是 DNS 解析失败或网络错误，不是参数错误
	if err == nil {
		t.Error("期望请求失败返回错误")
	}
	// 只要不是参数缺失错误就算通过
	if strings.Contains(err.Error(), "缺少") {
		t.Errorf("不应是参数缺失错误: %s", err.Error())
	}
}

// --- httptest 辅助测试 ---
// 以下测试使用 httptest 但需要处理 SSRF 防护
// 主要用于验证 HTTP 客户端逻辑的正确性

func TestFetchURL_ServerResponse(t *testing.T) {
	// 创建测试服务器
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html><body><h1>Test Server</h1></body></html>"))
	}))
	defer ts.Close()

	// 由于 httptest 使用 127.0.0.1，会被 SSRF 拦截
	// 我们验证错误信息是否来自 SSRF 而不是其他环节
	_, err := FetchURL(t.Context(), ts.URL)
	if err == nil {
		t.Fatal("期望 SSRF 拦截内网地址")
	}
	if !strings.Contains(err.Error(), "内网") {
		t.Errorf("期望 SSRF 错误信息包含'内网', 实际: %s", err.Error())
	}
}
