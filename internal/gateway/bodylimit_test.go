package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestParseJSONBodyRejectsOversized 兼容层同样不得静默截断超限请求体。
// 静默截断会把残缺 JSON 当成非法 JSON，报出与体积无关的 400。
func TestParseJSONBodyRejectsOversized(t *testing.T) {
	body := `{"model":"workbuddy/x","pad":"` + strings.Repeat("a", int(maxInnerBody)) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()

	if _, ok := parseJSONBody(rec, req, false); ok {
		t.Fatal("超限请求体不应解析成功")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", rec.Code, rec.Body.String())
	}
}

// TestParseJSONBodyAcceptsBodyAtLimit 边界值必须放行，避免差一错误误伤。
func TestParseJSONBodyAcceptsBodyAtLimit(t *testing.T) {
	const head = `{"model":"workbuddy/x","pad":"`
	const tail = `"}`
	body := head + strings.Repeat("a", int(maxInnerBody)-len(head)-len(tail)) + tail
	if len(body) != int(maxInnerBody) {
		t.Fatalf("测试构造错误: len=%d", len(body))
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()

	if _, ok := parseJSONBody(rec, req, false); !ok {
		t.Fatalf("边界值应解析成功，status=%d body=%s", rec.Code, rec.Body.String())
	}
}
