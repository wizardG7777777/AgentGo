package webtool

import "testing"

// AllowLoopbackForTests 临时禁用 SSRF 防护中的 loopback/private/link-local 拒绝逻辑，
// 让 httptest.NewServer 启动的 127.0.0.1 服务器可以被 FetchURL/FetchURLWithMode 访问。
//
// 用法：
//
//	func TestSomething(t *testing.T) {
//	    webtool.AllowLoopbackForTests(t)
//	    srv := httptest.NewServer(handler)
//	    defer srv.Close()
//	    // 现在 webtool.FetchURLWithMode(ctx, srv.URL, "auto") 不会被 SSRF 拦截
//	}
//
// 通过 t.Cleanup 在测试结束时自动复位，不会污染并发或后续测试。
//
// 安全约束：
//   - 仅 _test.go 文件可调用；接受 testing.TB 参数让生产代码无法误用
//   - 不要在并行测试（t.Parallel）中使用，因为它修改的是包级全局变量
//   - 仅放行 loopback/private/link-local 校验；其他 SSRF 防护手段（如未来加入的 URL scheme 检查）不受影响
func AllowLoopbackForTests(t testing.TB) {
	t.Helper()
	allowLoopbackForTests = true
	t.Cleanup(func() { allowLoopbackForTests = false })
}
