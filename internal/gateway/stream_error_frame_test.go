package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 本文件锁定「流内 error 帧不得被兼容层吞掉」（2026-10-03 复核发现的 high 问题）。
//
// 背景：内层 handler 在流中途遇到上游业务错误时，会发一帧 OpenAI 规范 error
// （如 {"error":{"code":"upstream_rate_limited",...}}）且**不补 [DONE]**。
// 但 gateway 的 parseChatSSELine 只认带 choices 的分片，error 帧被解析成空 chunk
// 后**静默丢弃**；内层又不写 [DONE]，于是 gateway 读到 EOF 就当正常结束：
//   - /v1/messages  → 只发 end_turn + message_stop（客户端以为答完了）
//   - /v1/responses → 还补 response.completed + data:[DONE]
// 客户端因此把半截回答当成完整回答继续跑 —— 正是流内错误要消灭的症状。
//
// 修复后：识别 error 帧 → 转成对应协议的失败终态，且不发成功收尾。
//
// ⚠️ 注意这类缺口对内层所有渠道通用（含 #42 的 upstream_truncated），
// 不只 traework，故本测试同时覆盖两个错误码。

// errFrameHandler 假内层：先透传一段内容，再发一帧 error，且**不补 [DONE]**
// （与内层 solosse/upstream 的错误路径逐字一致）。
type errFrameHandler struct {
	content string
	errJSON string
	// doneAfterErr 为 true 时错误帧后仍补 [DONE]（模拟"错误被伪装成正常收尾"的旧行为，
	// 用于对照）。
	doneAfterErr bool
}

func (h *errFrameHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	f, _ := w.(http.Flusher)
	chunk := func(delta string) string {
		return fmt.Sprintf(`data: {"id":"c1","object":"chat.completion.chunk","model":"up","choices":[{"index":0,"delta":{"content":%q}}]}`+"\n\n", delta)
	}
	_, _ = w.Write([]byte(chunk(h.content)))
	if f != nil {
		f.Flush()
	}
	_, _ = w.Write([]byte("data: " + h.errJSON + "\n\n"))
	if f != nil {
		f.Flush()
	}
	if h.doneAfterErr {
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if f != nil {
			f.Flush()
		}
	}
}

// 两帧真实错误：traework 限流（本次新增）与 #42 截断（master 既有，同样被吞过）。
func rateLimitedFrame() string {
	return `{"error":{"message":"solo error code=3004 msg=rate limited","type":"upstream_error","code":"upstream_rate_limited"}}`
}
func truncatedFrame() string {
	return `{"error":{"message":"upstream stream truncated","type":"upstream_error","code":"upstream_truncated"}}`
}

// TestAnthropicStreamErrorFrameNotSwallowed /v1/messages：流内 error 帧必须
// 变成 Anthropic 的 error 事件，且**不得**以 end_turn/message_stop 正常收尾。
func TestAnthropicStreamErrorFrameNotSwallowed(t *testing.T) {
	for name, frame := range map[string]string{"rate_limited": rateLimitedFrame(), "truncated": truncatedFrame()} {
		t.Run(name, func(t *testing.T) {
			inner := &errFrameHandler{content: "部分回答", errJSON: frame}
			_, mux := newTestGateway(inner)
			rec := post(mux, "/v1/messages",
				`{"model":"claude-sonnet-4","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
			body := rec.Body.String()

			if !strings.Contains(body, `"error"`) {
				t.Errorf("流内错误必须转成 Anthropic error 事件，实际 body=%s", body)
			}
			// 关键：不得出现"正常收尾"信号
			if strings.Contains(body, `"message_stop"`) {
				t.Errorf("错误流不得发 message_stop（会被当成正常收尾）\nbody=%s", body)
			}
			if strings.Contains(body, `"end_turn"`) {
				t.Errorf("错误流不得发 end_turn\nbody=%s", body)
			}
			// 已透传的部分内容不应丢
			if !strings.Contains(body, "部分回答") {
				t.Errorf("已透传内容不应丢失\nbody=%s", body)
			}
		})
	}
}

// TestResponsesStreamErrorFrameNotSwallowed /v1/responses：流内 error 帧必须
// 变成 response.failed，且**不得**发 response.completed / data:[DONE]。
func TestResponsesStreamErrorFrameNotSwallowed(t *testing.T) {
	for name, frame := range map[string]string{"rate_limited": rateLimitedFrame(), "truncated": truncatedFrame()} {
		t.Run(name, func(t *testing.T) {
			inner := &errFrameHandler{content: "部分回答", errJSON: frame}
			_, mux := newTestGateway(inner)
			rec := post(mux, "/v1/responses",
				`{"model":"claude-sonnet-4","stream":true,"input":"hi"}`)
			body := rec.Body.String()

			if !strings.Contains(body, "response.failed") {
				t.Errorf("流内错误必须转成 response.failed，实际 body=%s", body)
			}
			if strings.Contains(body, "response.completed") {
				t.Errorf("错误流不得发 response.completed（会被当成成功）\nbody=%s", body)
			}
			if strings.Contains(body, "[DONE]") {
				t.Errorf("错误流不得补 [DONE]\nbody=%s", body)
			}
			if !strings.Contains(body, "部分回答") {
				t.Errorf("已透传内容不应丢失\nbody=%s", body)
			}
		})
	}
}

// TestParseChatSSELineRecognizesError 单元级：error 帧必须被识别为错误分片，
// 而不是"空分片"（修复前它解析成功但内容全空，被调用方静默跳过）。
func TestParseChatSSELineRecognizesError(t *testing.T) {
	c, ok := parseChatSSELine("data: " + rateLimitedFrame())
	if !ok {
		t.Fatal("error 帧应被识别（ok=true），否则会被静默丢弃")
	}
	if c.Err == nil {
		t.Fatal("error 帧必须带 Err（否则调用方无从判错）")
	}
	if c.Err.Code != "upstream_rate_limited" {
		t.Errorf("错误码=%q want upstream_rate_limited", c.Err.Code)
	}
	if !strings.Contains(c.Err.Error(), "rate limited") {
		t.Errorf("错误信息应保留上游原文，got %q", c.Err.Error())
	}
	// 正常分片不得被误判为错误
	normal := `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"hi"}}]}`
	nc, ok := parseChatSSELine(normal)
	if !ok || nc.Err != nil || nc.Content != "hi" {
		t.Errorf("正常分片解析异常: ok=%v err=%v content=%q", ok, nc.Err, nc.Content)
	}
}

// TestErrorFrameAlsoFixesLegacyDisguisedDone 对照组：即使内层把错误**伪装**成
// 正常收尾（错误帧后补了 [DONE]，即修复前的行为），gateway 也应因为先看到
// error 帧而走失败终态——错误帧优先于 [DONE]。
func TestErrorFrameAlsoFixesLegacyDisguisedDone(t *testing.T) {
	inner := &errFrameHandler{content: "部分回答", errJSON: rateLimitedFrame(), doneAfterErr: true}
	_, mux := newTestGateway(inner)
	rec := post(mux, "/v1/messages",
		`{"model":"claude-sonnet-4","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	body := rec.Body.String()
	if !strings.Contains(body, `"error"`) {
		t.Errorf("error 帧应优先于其后的 [DONE]\nbody=%s", body)
	}
	if strings.Contains(body, `"message_stop"`) {
		t.Errorf("不得发 message_stop\nbody=%s", body)
	}
}

// anthropicErrTypes 是 Anthropic SDK ErrorObject 的 9 元判别联合（type 为 Literal 标签）。
// 服务端**不得自造** type —— 严格客户端会因判别标签落空而判为未知类型。
// 依据：Anthropic Python SDK error_object.py / api_error_object.py / rate_limit_error.py。
var anthropicErrTypes = map[string]bool{
	"invalid_request_error": true,
	"authentication_error":  true,
	"billing_error":         true,
	"permission_error":      true,
	"not_found_error":       true,
	"rate_limit_error":      true,
	"gateway_timeout_error": true,
	"api_error":             true,
	"overloaded_error":      true,
}

// extractAnthropicErrType 从 SSE body 里取出 error 事件的 error.type。
func extractAnthropicErrType(t *testing.T, body string) string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) != nil {
			continue
		}
		if ev.Type == "error" && ev.Error.Type != "" {
			return ev.Error.Type
		}
	}
	return ""
}

// TestAnthropicErrorTypeIsSpecEnum 回归（第二轮复核发现）：
// 第一版整改把 error.type 填成了内层私有码 upstream_rate_limited / upstream_truncated，
// 不在 SDK 的 9 元判别联合内 —— 这是**方向性退步**（最初就是 api_error）。
// 本测试钉死：type 必须落在枚举内；限流语义须映射为 rate_limit_error。
func TestAnthropicErrorTypeIsSpecEnum(t *testing.T) {
	cases := []struct {
		name     string
		frame    string
		wantType string
	}{
		{"限流 → rate_limit_error", rateLimitedFrame(), "rate_limit_error"},
		{"截断 → api_error（通用）", truncatedFrame(), "api_error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			inner := &errFrameHandler{content: "部分回答", errJSON: c.frame}
			_, mux := newTestGateway(inner)
			rec := post(mux, "/v1/messages",
				`{"model":"claude-sonnet-4","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
			got := extractAnthropicErrType(t, rec.Body.String())

			if got == "" {
				t.Fatalf("未找到 error 事件的 error.type\nbody=%s", rec.Body.String())
			}
			if !anthropicErrTypes[got] {
				t.Errorf("error.type=%q 不在 Anthropic 9 元判别联合内（服务端不得自造 type）", got)
			}
			if got != c.wantType {
				t.Errorf("error.type=%q want %q", got, c.wantType)
			}
			// 内层私有码必须仍可见（挪到 message 里，信息不丢）
			if !strings.Contains(rec.Body.String(), "3004") && !strings.Contains(rec.Body.String(), "truncated") {
				t.Errorf("内层错误信息不应丢失\nbody=%s", rec.Body.String())
			}
		})
	}
}

// errAfterReader 先给出若干字节，然后返回一个**非 EOF 的读错误**，
// 模拟「内层流写到一半中断」（io.Pipe 下读端会拿到 ErrClosedPipe）。
// 之所以直接构造 innerResult 而不走假 Inner handler：内层 handler 正常返回时
// io.Pipe 会以 EOF 收尾（被 iterateChatSSE 归一为 nil），触发不了错误路径。
type errAfterReader struct {
	data []byte
	done bool
	err  error
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	if !r.done {
		r.done = true
		return 0, r.err
	}
	return 0, io.EOF
}
func (r *errAfterReader) Close() error { return nil }

// TestResponsesErrorEventCodeNeverEmpty 回归（第三轮复核发现）：
// error 事件的自由 code 对**非流内错误**（读错误/中断）必须非空。
//
// 第一版整改把固定的 "stream_error" 换成了 streamErrCode(err)，而后者对
// 非 *StreamError 返回空串 ⇒ 真实中断场景会发出 "code":""，
// 把原来（固定 "stream_error"）的诊断信息丢掉。规范上空串类型合法
// （Optional[str]），但按 code 分支的客户端从此拿不到任何线索。
func TestResponsesErrorEventCodeNeverEmpty(t *testing.T) {
	upErr := errors.New("simulated upstream read failure")
	g := New(Config{Inner: &scriptedHandler{}, Router: testRouter()})
	res := &innerResult{
		Status: http.StatusOK,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   &errAfterReader{data: []byte("部分回答"), err: upErr},
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	g.streamResponses(rec, req, res, "claude-sonnet-4", chatRequest{"model": "claude-sonnet-4"})
	body := rec.Body.String()

	var gotCode string
	var sawErrEvent bool
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Type string `json:"type"`
			Code string `json:"code"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) != nil {
			continue
		}
		if ev.Type == "error" {
			sawErrEvent = true
			gotCode = ev.Code
		}
	}
	if !sawErrEvent {
		t.Fatalf("读错误必须发出 error 事件\nbody=%s", body)
	}
	if gotCode == "" {
		t.Errorf("error 事件的 code 不得为空（会丢掉诊断信息）\nbody=%s", body)
	}
	if strings.Contains(body, `"code":""`) {
		t.Errorf("不得出现空 code 字段\nbody=%s", body)
	}
	if !strings.Contains(body, "response.failed") {
		t.Errorf("读错误也应以 response.failed 收尾（不得伪装成功）\nbody=%s", body)
	}
}

// 依据：OpenAI Python SDK response_error.py（21 元 Literal，官方标注会扩展）。
// ⚠️ 只列规范内实际存在的值——早期版本曾误把 invalid_request / not_found
// 写进来（它们不在该枚举内），使这条「对照规范」的断言名不副实。
var responseErrEnum = map[string]bool{
	"server_error":         true,
	"rate_limit_exceeded":  true,
	"invalid_prompt":       true,
	"vector_store_timeout": true,
}

// TestResponsesErrorCodeIsSpecEnum 同上的 Responses 侧：
// response.error.code 须映射到枚举内；内层私有码放在 error 事件的 code 字段。
func TestResponsesErrorCodeIsSpecEnum(t *testing.T) {
	cases := []struct {
		name     string
		frame    string
		wantCode string
	}{
		{"限流 → rate_limit_exceeded", rateLimitedFrame(), "rate_limit_exceeded"},
		{"截断 → server_error", truncatedFrame(), "server_error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			inner := &errFrameHandler{content: "部分回答", errJSON: c.frame}
			_, mux := newTestGateway(inner)
			rec := post(mux, "/v1/responses", `{"model":"claude-sonnet-4","stream":true,"input":"hi"}`)
			body := rec.Body.String()

			// 取 response.failed 里的 response.error.code
			var got string
			for _, line := range strings.Split(body, "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				var ev struct {
					Type     string `json:"type"`
					Response struct {
						Error struct {
							Code string `json:"code"`
						} `json:"error"`
					} `json:"response"`
				}
				if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) != nil {
					continue
				}
				if ev.Type == "response.failed" {
					got = ev.Response.Error.Code
				}
			}
			if got == "" {
				t.Fatalf("未取到 response.failed 的 error.code\nbody=%s", body)
			}
			if !responseErrEnum[got] {
				t.Errorf("response.error.code=%q 不在 ResponseError 枚举内（应映射，不得直填内层码）", got)
			}
			if got != c.wantCode {
				t.Errorf("error.code=%q want %q", got, c.wantCode)
			}
			// 内层私有码须在 error 事件（自由码载体）里可见
			if !strings.Contains(body, "upstream_") {
				t.Errorf("内层私有码应在 error 事件里保留\nbody=%s", body)
			}
		})
	}
}
