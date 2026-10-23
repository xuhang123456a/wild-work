package traework

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStreamUsageReturnedBeforeDone 流式 usage 返回值回归（token 流水记账）。
//
// SOLO 协议事件序：output → token_usage → done。旧实现 done 分支先 writeChunk
// 再取 pendingUsage——writeChunk 会把 pendingUsage 附加进末 chunk 后置 nil，
// 导致 Stream 返回的 usage 恒为空：客户端能收到 usage（已透传），但网关记账
// 落库 pt=ct=0、src=none（v2.5.2 起 Trae 系 token 流水全丢的根因）。
// 修复：done 分支先 usage = pendingUsage 再 writeChunk。
func TestStreamUsageReturnedBeforeDone(t *testing.T) {
	sse := "event: metadata\ndata: {\"id\":\"abc\"}\n\n" +
		"event: output\ndata: {\"response\":\"hi\"}\n\n" +
		"event: token_usage\ndata: {\"prompt_tokens\":17,\"completion_tokens\":73}\n\n" +
		"event: done\ndata: {\"finish_reason\":\"stop\"}\n\n"

	w := httptest.NewRecorder()
	usage, err := StreamWithModel(w, strings.NewReader(sse), "glm-5.3")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if usage == nil {
		t.Fatal("Stream 返回 usage=nil：客户端已收到 usage chunk，记账侧不应为空")
	}
	pt, okPt := usage["prompt_tokens"].(float64)
	ct, okCt := usage["completion_tokens"].(float64)
	if !okPt || !okCt || pt != 17 || ct != 73 {
		t.Fatalf("usage=%v，期望 prompt_tokens=17 completion_tokens=73", usage)
	}
	// 客户端侧不受影响：末 chunk 仍带 usage 透传
	body := w.Body.String()
	if !strings.Contains(body, `"usage"`) || !strings.Contains(body, `"prompt_tokens":17`) {
		t.Fatal("客户端侧 usage 透传被破坏")
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatal("缺少 [DONE]")
	}
}

// TestStreamNoUsageStillFine 上游不发 token_usage 时返回 nil（不误造）。
func TestStreamNoUsageStillFine(t *testing.T) {
	sse := "event: output\ndata: {\"response\":\"hi\"}\n\n" +
		"event: done\ndata: {\"finish_reason\":\"stop\"}\n\n"
	w := httptest.NewRecorder()
	usage, err := StreamWithModel(w, strings.NewReader(sse), "glm-5.3")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if usage != nil {
		t.Fatalf("无 token_usage 时 usage 应为 nil，got %v", usage)
	}
}
