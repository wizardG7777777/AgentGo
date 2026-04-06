package webtool

import (
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

func TestFetchURL_MissingURL(t *testing.T) {
	_, err := FetchURL(t.Context(), "")
	if err == nil {
		t.Error("期望空 URL 返回错误")
	}
	if !strings.Contains(err.Error(), "缺少 url 参数") {
		t.Errorf("错误信息不正确: %s", err.Error())
	}
}

func TestFetchURL_PrependHTTPS(t *testing.T) {
	// 验证非 http/https URL 会被加上 https:// 前缀
	// 使用一个不会解析成功的域名来触发网络错误（而非参数校验错误）
	_, err := FetchURL(t.Context(), "this-domain-does-not-exist-1234567890.invalid")
	if err == nil {
		t.Error("期望请求失败域名返回错误")
	}
	// 错误应该是请求失败（网络错误），不是参数缺失
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

// --- SSRF 防护测试 ---

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
