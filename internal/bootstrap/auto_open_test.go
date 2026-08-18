package bootstrap

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentgo/internal/config"
	"agentgo/internal/dashboard"
)

// TestMaybeAutoOpenBrowser_OpensWithToken 服务就绪后应以带 ?token= 的地址
// 拉起默认浏览器（OpenBrowser 已替换为测试桩）。
func TestMaybeAutoOpenBrowser_OpensWithToken(t *testing.T) {
	var got atomic.Value
	orig := dashboard.OpenBrowser
	dashboard.OpenBrowser = func(url string) error { got.Store(url); return nil }
	defer func() { dashboard.OpenBrowser = orig }()

	srv := dashboard.NewServer(nil, "127.0.0.1:0", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()

	// Addr() 在 listen 成功前返回空串，先等真正绑定
	var boundAddr string
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		if boundAddr = srv.Addr(); boundAddr != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if boundAddr == "" {
		t.Fatal("dashboard 3s 内未完成 listen")
	}

	cfg := &config.Config{}
	cfg.UI.Frontends = []string{"web"}
	cfg.UI.Web.Listen = boundAddr
	cfg.UI.Web.Token = "t0ken 值"
	on := true
	cfg.UI.Web.AutoOpen = &on
	s := &System{Config: cfg}
	s.maybeAutoOpenBrowser(ctx)

	deadline := time.After(4 * time.Second)
	for {
		if v := got.Load(); v != nil {
			url := v.(string)
			if !strings.HasPrefix(url, "http://"+boundAddr+"/?token=") {
				t.Fatalf("打开地址缺少 token: %q", url)
			}
			if !strings.Contains(url, "t0ken+%E5%80%BC") && !strings.Contains(url, "t0ken%20%E5%80%BC") {
				t.Fatalf("token 未经 QueryEscape: %q", url)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("4s 内未拉起浏览器")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// TestMaybeAutoOpenBrowser_Disabled auto_open: false 时不拉起浏览器。
func TestMaybeAutoOpenBrowser_Disabled(t *testing.T) {
	called := atomic.Int32{}
	orig := dashboard.OpenBrowser
	dashboard.OpenBrowser = func(url string) error { called.Add(1); return nil }
	defer func() { dashboard.OpenBrowser = orig }()

	off := false
	cfg := &config.Config{}
	cfg.UI.Web.Listen = "127.0.0.1:1"
	cfg.UI.Web.AutoOpen = &off
	s := &System{Config: cfg}
	s.maybeAutoOpenBrowser(context.Background())
	time.Sleep(200 * time.Millisecond)
	if called.Load() != 0 {
		t.Fatal("auto_open: false 时不应拉起浏览器")
	}
}
