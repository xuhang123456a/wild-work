package cdp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestAttachReadsBrowserWSEndpoint 锁死「接管已有浏览器」的第一步：
// 从 /json/version 拿到浏览器级 WS 地址，且 Close 绝不关掉用户的浏览器。
func TestAttachReadsBrowserWSEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/version" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"Browser":"Chrome/152.0.0.0","webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/browser/abc"}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, err := Attach(ctx, srv.URL)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if !b.attached {
		t.Fatal("attached 必须为 true（否则 Close 会杀掉用户的浏览器）")
	}
	if got := b.browserWSURL(); got != "ws://127.0.0.1:9222/devtools/browser/abc" {
		t.Fatalf("browserWSURL = %q", got)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestAttachRejectsDeadEndpoint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := Attach(ctx, "http://127.0.0.1:1"); err == nil {
		t.Fatal("调试端口不通时应报错")
	} else if !strings.Contains(err.Error(), "调试端口未响应") {
		t.Fatalf("错误应指明调试端口不可用，实际: %v", err)
	}
}
