package comate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"wild-work/internal/provider"
	"wild-work/internal/upstream"
)

// ---------------------------------------------------------------------------
// peekStream：窥视首帧，识别 mock 错误帧
// ---------------------------------------------------------------------------

// peekStream 包裹上游 body：边读边窥视首段（至多 64KB）判断是否 mock 错误帧
//（id 前缀 chatcmpl-mock- 或 model=="mock"，见包注释实测事实 2），
// 命中则转成可分类的 StreamError（模型级 ErrNotFound，不罚整号）；
// 未命中则把已读字节即时交给调用方（**绝不攒批**）。
//
// 为什么必须在流上做：mock 帧的 HTTP 状态是 200、响应头正常，handler 在
// 读到首帧之前就已把 200 + SSE 头写给客户端 —— 协议上无法在同一请求内
// 换号重试，唯一可行的补救是把中招模型冷却掉（与 StreamErrorClassifier
// 的设计动机一致）。
//
// ⚠️ 窥视必须是**增量**的（2026-10-28 修正）：旧实现 `io.ReadAll(io.LimitReader(body, 64<<10))`
// 要读满 64KB 或 EOF 才返回，实测首帧 flush 后上游停 1.5s，客户端首次 Read
// 也被推迟 1.5024s —— 所有 comate 对话都退化成「正文突然整段出现」；且 IdleReader
// 包在窥视之后，首字节前的空闲无任何兜底（上游 200 后不发字节会永久挂住）。
// 现在判定窗口是**首帧边界**（SSE 的 "\n\n"）或 64KB 上限，任一先到即判定，
// 且底层 body 一开始就包上 IdleReader —— 空闲兜底覆盖「首字节之前」。
type peekStream struct {
	src     io.ReadCloser // 底层流（已包 IdleReader）
	pre     []byte        // 已窥视但未交付的字节（每次 Read 交付完即清）
	head    []byte        // 判定窗口累积（最多 peekLimit）
	decided bool          // 是否已判定
	model   string
}

// peekLimit 判定窗口上限：单帧正常远小于此，64KB 是防「上游不按 SSE 分帧」的兜底。
const peekLimit = 64 << 10

func newPeekStream(body io.ReadCloser, model string, idle time.Duration) *peekStream {
	// 底层一开始就包 IdleReader：窥视期间的空闲同样受兜底（旧实现漏了这一段）。
	return &peekStream{src: provider.NewIdleReader(body, idle), model: model}
}

// Read 首次调用时边读边判定，之后退化为纯透传。
// 判定窗口内若识别出 mock 帧，后续所有 Read 都返回 StreamError（已窥视字节**不交付**）。
func (p *peekStream) Read(b []byte) (int, error) {
	if !p.decided {
		if err := p.decide(); err != nil {
			return 0, err
		}
	}
	if len(p.pre) > 0 {
		n := copy(b, p.pre)
		p.pre = p.pre[n:]
		return n, nil
	}
	return p.src.Read(b)
}

// decide 增量读取直到「首帧完整」或 peekLimit，再判 mock。
// 交付延迟上限 = 首帧到达时刻（不是流结束时刻）。
func (p *peekStream) decide() error {
	buf := make([]byte, 4096)
	for !p.decided {
		n, err := p.src.Read(buf)
		if n > 0 {
			p.head = append(p.head, buf[:n]...)
			p.pre = append(p.pre, buf[:n]...)
		}
		// 判定点：首帧边界（\n\n）已出现，或窗口达到上限，或底层 EOF。
		if bytes.Contains(p.head, []byte("\n\n")) || len(p.head) >= peekLimit || err != nil {
			p.decided = true
			if isMockFrame(p.head) {
				// 命中 mock：丢弃已窥视字节（不把错误文案当回答吐出去），
				// 剩余流不再读（Close 会关底层）。
				msg := extractMockMsg(p.head)
				p.pre, p.head = nil, nil
				_ = p.src.Close()
				return &StreamError{Model: p.model, Code: "MOCK_FRAME", Msg: msg, kind: provider.ErrNotFound}
			}
			p.head = nil // 非 mock：判定窗口释放，pre 留给 Read 交付
		}
		if err != nil {
			// 底层错误/EOF：非 mock 时把已读字节先交付（下次 Read 再返回 err）。
			if len(p.pre) > 0 {
				return nil
			}
			return err
		}
	}
	return nil
}

func (p *peekStream) Close() error {
	return p.src.Close()
}

// isMockFrame 判据（实测事实 2）：id 前缀 chatcmpl-mock- 或 model=="mock"。
// 两个判据都查：上游形态可能变化，双保险。
func isMockFrame(head []byte) bool {
	s := string(head)
	return strings.Contains(s, "chatcmpl-mock-") || strings.Contains(s, `"model":"mock"`)
}

// extractMockMsg 从 mock 帧提取错误文案（delta.content 或 msg 字段）。
func extractMockMsg(head []byte) string {
	// 形态 1：SSE 帧 {"choices":[{"delta":{"content":"…"}}]}
	var frame struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(head, &frame); err == nil && len(frame.Choices) > 0 && frame.Choices[0].Delta.Content != "" {
		return frame.Choices[0].Delta.Content
	}
	// 形态 2：非流式信封 {"code":"…","msg":"…"}
	var env struct {
		Msg string `json:"msg"`
	}
	if err := json.Unmarshal(head, &env); err == nil && env.Msg != "" {
		return env.Msg
	}
	return truncate(string(head), 256)
}

// sha256Sum 返回 hex 摘要（uid 生成用）。
func sha256Sum(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// Stream / Aggregate（provider.Upstream 接口方法）
// ---------------------------------------------------------------------------

// Stream 实现 provider.Upstream：逐帧透传上游 SSE，同时捕获流内 mock 错误帧
// 并转成可分类的 error（供 handler 冷却中招模型，R40 同款）。
func (c *Client) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	var usage map[string]any
	var frame string

	serr := upstream.StreamWith(w, r,
		func(payload string) string {
			// hintFn 只在 error 帧上被调用；这里只取样、不附加 hint（返回空串 ⇒ 原文透传）。
			if frame == "" && !isLocalErrFrame(payload) {
				frame = payload
			}
			return ""
		},
		func(u map[string]any) { usage = u })

	if frame != "" {
		se := parseErrFrame(frame)
		if se != nil {
			se.Model = model
			log.Printf("comate: 流内 error 帧 uid=- model=%s code=%s msg=%.180s", model, se.Code, se.Msg)
			if serr == nil {
				return usage, se
			}
		}
	}
	return usage, serr
}

// Aggregate 实现 provider.Upstream（客户端 stream=false 时由 handler 调用）。
// 上游永远被请求 stream:true（不变量 1），聚合走流式帧；error 帧同样要识别
//（否则非流式客户端拿到空 choices 且不报错，R40 的第二个入口）。
// 响应 model 字段是短名（实测事实 3），回填客户端请求的原始模型名（R14 同款）。
func (c *Client) Aggregate(r io.Reader, model string) (map[string]any, error) {
	raw, err := io.ReadAll(io.LimitReader(r, 32<<20))
	if err != nil {
		return nil, err
	}
	se := scanAggregateErrFrame(raw)
	res, aerr := upstream.Aggregate(bytes.NewReader(raw))
	if res != nil {
		// 回填调用方请求的原始模型名（上游常回短名，R14 同款显式传参）。
		res["model"] = model
	}
	if se != nil {
		se.Model = model
		log.Printf("comate: 聚合流内 error 帧 model=%s code=%s msg=%.180s", model, se.Code, se.Msg)
		if aerr == nil {
			return res, se
		}
	}
	return res, aerr
}

// scanAggregateErrFrame 在已读尽的 SSE 文本里找第一帧**上游** error。
func scanAggregateErrFrame(raw []byte) *StreamError {
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		if isLocalErrFrame(payload) {
			continue
		}
		if se := parseErrFrame(payload); se != nil {
			return se
		}
	}
	return nil
}

// isLocalErrFrame 判定「本地合成」的 error 帧（upstream 包在流尾注入的
// upstream_truncated / upstream_parse 等）——它们不是上游说的，不得二次归类。
func isLocalErrFrame(payload string) bool {
	for _, m := range localErrMarkers {
		if strings.Contains(payload, m) {
			return true
		}
	}
	return false
}

// localErrMarkers 本地合成帧的标记（与 cline 同款）。
var localErrMarkers = []string{
	"upstream_truncated", "upstream_parse", "empty upstream stream",
}

// parseErrFrame 把上游 error 帧解析成 StreamError。
// comate 的 error 帧形态：{"error":{"message":"…","code":"…"}}（OpenAI 兼容）。
func parseErrFrame(payload string) *StreamError {
	var f struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(payload), &f); err != nil || f.Error.Message == "" {
		return nil
	}
	se := &StreamError{Code: f.Error.Code, Msg: f.Error.Message}
	if se.Code == "" {
		se.Code = "unknown"
	}
	// 文案分类（与 Classify 同一套 markers）。
	switch {
	case IsSessionDeadBody(f.Error.Message):
		se.kind = provider.ErrSessionDead
	case IsHardCreditBody(f.Error.Message):
		se.kind = provider.ErrHardCredit
	case IsModelUnavailableBody(f.Error.Message):
		se.kind = provider.ErrNotFound
	}
	return se
}
