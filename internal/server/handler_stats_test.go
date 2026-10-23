package server

import (
	"net/http/httptest"
	"testing"
	"time"
)

// TestStatWriterRecordsTTFB 首字节计一次即定（WriteHeader/Write/Flush 先到先记，不重记）。
func TestStatWriterRecordsTTFB(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statWriter{ResponseWriter: rec, start: time.Now()}
	time.Sleep(5 * time.Millisecond)
	sw.WriteHeader(200)
	if !sw.written {
		t.Fatal("WriteHeader 后应标记首字节")
	}
	if sw.ttfbMs < 4 {
		t.Fatalf("TTFB 应 ≥4ms（sleep 5ms），实际 %d", sw.ttfbMs)
	}
	first := sw.ttfbMs
	_, _ = sw.Write([]byte("x"))
	if sw.ttfbMs != first {
		t.Fatalf("首字节后不应重记：%d → %d", first, sw.ttfbMs)
	}
}

// TestStatWriterFlushCounts 流式场景：首个 Flush 即首字节时机（SSE 首帧先 flush 后有 body）。
func TestStatWriterFlushCounts(t *testing.T) {
	rec := httptest.NewRecorder() // ResponseRecorder 实现 http.Flusher
	sw := &statWriter{ResponseWriter: rec, start: time.Now()}
	time.Sleep(5 * time.Millisecond)
	sw.Flush()
	if !sw.written || sw.ttfbMs < 4 {
		t.Fatalf("Flush 应记首字节，实际 written=%v ttfbMs=%d", sw.written, sw.ttfbMs)
	}
}

// TestUsageCredit 单次积分提取：WorkBuddy 系下发 float；缺失/非数值 → 0。
func TestUsageCredit(t *testing.T) {
	if got := usageCredit(map[string]any{"credit": 12.5}); got != 12.5 {
		t.Fatalf("应提取 12.5，实际 %v", got)
	}
	if got := usageCredit(map[string]any{"prompt_tokens": 10.0}); got != 0 {
		t.Fatalf("无 credit 字段应 0，实际 %v", got)
	}
	if got := usageCredit(map[string]any{"credit": "12.5"}); got != 0 {
		t.Fatalf("非数值 credit 应 0，实际 %v", got)
	}
	if got := usageCredit(nil); got != 0 {
		t.Fatalf("nil usage 应 0，实际 %v", got)
	}
}
