// request.go 把 wild-work 内部的 OpenAI Chat 请求体翻译成 Anthropic Messages 请求体。
//
// wild-work 的架构是「外层三接口（Chat / Responses / Anthropic）统一收敛成
// OpenAI Chat 形状，再交给渠道」；而 MonkeyCode 上游是 **Anthropic 形状**
// （客户端 settings.json 里 type=anthropic），因此本渠道需要一层请求方言投影。
//
// 出站体形态（与官方客户端实测一致，见评估文档 §3.6）：
//
//	{model, max_tokens, system:[{type:text,text:…}…], messages:[…], stream, thinking}
//
// system[0] 是**签名对象**（固定常量，见 sign.go）；客户端的 system 内容一律
// 追加在 system[1] 之后 —— 后续条目不参与签名，可自由携带动态内容。
package monkeycode

import (
	"encoding/json"
	"fmt"
	"strings"
)

// buildAnthropicBody 把 OpenAI Chat 请求体翻译成 Anthropic Messages 请求体。
func buildAnthropicBody(raw []byte) ([]byte, error) {
	var in map[string]any
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("monkeycode: invalid request body: %w", err)
	}

	// 模型名：入参已由 handler 剥掉渠道前缀（`monkeycode/basic/x` → `basic/x`），
	// 这里补回上游要求的 `monkeycode-` 前缀。
	model, _ := in["model"].(string)
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, fmt.Errorf("monkeycode: missing model")
	}
	model = upstreamModelName(model)

	// system：第一条恒为签名对象，其余为客户端的 system 内容。
	system := []any{map[string]any{"type": "text", "text": signatureSystemPrompt}}
	messages, sysTexts, err := convertMessages(in["messages"])
	if err != nil {
		return nil, err
	}
	for _, s := range sysTexts {
		if strings.TrimSpace(s) == "" {
			continue
		}
		system = append(system, map[string]any{"type": "text", "text": s})
	}

	out := map[string]any{
		"model":      model,
		"max_tokens": maxTokens(in),
		"system":     system,
		"messages":   messages,
		// 恒取 SSE，**不**透传客户端的 stream 字段。
		// 渠道层契约是「ChatStream 返回的一定是 SSE 流」，非流式客户端由网关用
		// Aggregate 收敛（internal/server/handler.go:558/633）。若透传 false，
		// 上游会回**单个 JSON 对象**，被 SSE 转换器当作非 `data:` 行整包丢弃
		// → 静默的空 content + 0 usage（2026-09-24 实测踩到）。
		"stream":   true,
		"thinking": thinkingFor(in),
	}
	if v, ok := in["temperature"]; ok {
		out["temperature"] = v
	}
	if v, ok := in["top_p"]; ok {
		out["top_p"] = v
	}
	if v, ok := in["stop"]; ok {
		// Anthropic 只接受数组形态的 stop_sequences（字符串单值需包成数组）
		switch t := v.(type) {
		case string:
			if t != "" {
				out["stop_sequences"] = []any{t}
			}
		case []any:
			if len(t) > 0 {
				out["stop_sequences"] = t
			}
		}
	}
	// `tool_choice: "none"` 时必须**整个不下发 tools**：Anthropic 没有 none 语义，
	// 只发 tools 而不发 tool_choice 时模型照样会调工具（实测 finish_reason=tool_calls），
	// 直接违反 OpenAI 契约。两面统一按「裁掉 tools」处理，不依赖上游是否尊重 none。
	if tools := convertTools(in["tools"]); len(tools) > 0 && !toolsDisabled(in["tool_choice"]) {
		out["tools"] = tools
		if tc := convertToolChoice(in["tool_choice"]); tc != nil {
			out["tool_choice"] = tc
		}
	}
	return json.Marshal(out)
}

// toolsDisabled 报告客户端是否用 `tool_choice: "none"` 明确要求不要调用工具。
// 命中时调用方必须裁掉整个 tools 字段（见 buildAnthropicBody / buildResponsesBody）。
func toolsDisabled(v any) bool {
	s, _ := v.(string)
	return s == "none"
}

// reasoningLevel 取漏斗归一后的顶层 `reasoning_effort`（小写）。
// 空串表示客户端未表达思考意图（渠道未在 SupportsEffortKind 中，不会注入默认档）。
func reasoningLevel(in map[string]any) string {
	s, _ := in["reasoning_effort"].(string)
	return strings.ToLower(strings.TrimSpace(s))
}

// thinkingFor 把顶层 `reasoning_effort` 投影成 Anthropic 面的 `thinking` 开关。
//
// 2026-09-24 VM 真实上游实测（详见评估文档 §3.13）：
//   - 上游**默认开着**思考（不传 thinking：64 个 thinking 帧 / output 69）；
//   - `{type:"disabled"}` 确实关掉（0 帧 / output 14）；
//   - `{type:"enabled"}` 与「不传」等价（65 帧 / output 70）；
//   - `budget_tokens` 被接受但**对思考量无可测影响**（1024 → 59 帧/64 out，
//     与不带 budget 的 65 帧/70 out 同量级），故不下发；
//   - `effort` 是否上线**取决于模型能力**（2026-09-25 复测更正）：官方客户端按内置
//     目录的 `thinking_control` 分型——`binary` 型丢弃 effort（low/high 产出的 body
//     逐字节相同），`effort` 型则发 `{type:"adaptive"}` + `output_config.effort`。
//     本渠道 25 个托管模型里 anthropic 面仅 kimi-k2.6 属 effort 型（pro 档）。
//
// 因此本面只能表达「开 / 关」两态，没有梯度：
//   - 客户端明确表达任何档位（`low`/`medium`/`high`…）→ 开；
//   - `none` → 关；
//   - **未表达 → 开**（2026-09-24 拍板「两面未表达即开」，与 responses 面统一，
//     也与官方客户端默认形态一致；此处**显式**补 `enabled`，不依赖上游默认值不变）。
//
// ⚠️ 已知缺口：effort 型模型客户端走 `{type:"adaptive"}` + `output_config.effort`，
// 本函数会把档位压成 `enabled` 丢掉。是否补取决于上游是否真认 `output_config.effort`
// （未验证，见评估文档 §3.13）。
//
// 注：R19「不在客户端未表达时强行开思考」是**积分/DeepSeek 系**渠道的取舍（省额度），
// 不适用于本渠道 —— 上游默认本就开着，不补字段同样是开，显式补只是把契约写死。
func thinkingFor(in map[string]any) map[string]any {
	if reasoningLevel(in) == "none" {
		return map[string]any{"type": "disabled"}
	}
	return map[string]any{"type": "enabled"}
}

// maxTokens 取 max_tokens / max_completion_tokens，缺失或非法时回默认值，
// 并夹到 [1, maxTokensCap]。Anthropic 的 max_tokens 是必填字段。
func maxTokens(in map[string]any) int64 {
	n, ok := numField(in, "max_tokens")
	if !ok {
		n, ok = numField(in, "max_completion_tokens")
	}
	if !ok || n <= 0 {
		return maxTokensDefault
	}
	if n > maxTokensCap {
		return maxTokensCap
	}
	return n
}

func numField(in map[string]any, key string) (int64, bool) {
	switch v := in[key].(type) {
	case float64:
		return int64(v), true
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return i, true
		}
	case int:
		return int64(v), true
	case int64:
		return v, true
	}
	return 0, false
}

// convertMessages 把 OpenAI 的 messages 翻译成 Anthropic messages，
// 同时把 role=system 的内容抽出来单独返回（调用方放到 system[1..]）。
//
// Anthropic 的硬约束：messages 必须以 user 开头、角色需交替；本函数通过
// 「合并相邻同角色块」与「首条非 user 时补一条空 user」来满足。
func convertMessages(raw any) (out []any, system []string, err error) {
	list, _ := raw.([]any)
	type msg struct {
		role    string
		content []any
	}
	var merged []msg

	appendBlocks := func(role string, blocks []any) {
		if len(blocks) == 0 {
			return
		}
		if n := len(merged); n > 0 && merged[n-1].role == role {
			merged[n-1].content = append(merged[n-1].content, blocks...)
			return
		}
		merged = append(merged, msg{role: role, content: blocks})
	}

	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		switch role {
		case "system", "developer":
			if s, ok := m["content"].(string); ok {
				system = append(system, s)
			} else if blocks := textBlocks(m["content"]); len(blocks) > 0 {
				for _, b := range blocks {
					if bm, ok := b.(map[string]any); ok {
						if t, ok := bm["text"].(string); ok {
							system = append(system, t)
						}
					}
				}
			}
		case "assistant":
			appendBlocks("assistant", assistantBlocks(m))
		case "tool", "function":
			appendBlocks("user", toolResultBlocks(m))
		default: // user 及其它未知角色按 user 处理
			appendBlocks("user", userBlocks(m))
		}
	}

	if len(merged) == 0 {
		return nil, system, fmt.Errorf("monkeycode: 请求里没有可用消息")
	}
	// Anthropic 要求首条是 user：assistant 开头时补一条占位 user。
	if merged[0].role != "user" {
		merged = append([]msg{{role: "user", content: []any{map[string]any{"type": "text", "text": "(continue)"}}}}, merged...)
	}
	out = make([]any, 0, len(merged))
	for _, m := range merged {
		if len(m.content) == 0 {
			continue
		}
		out = append(out, map[string]any{"role": m.role, "content": m.content})
	}
	return out, system, nil
}

// userBlocks 转换 user 消息的 content（字符串或块数组）。
func userBlocks(m map[string]any) []any {
	var out []any
	for _, b := range contentBlocks(m["content"]) {
		switch v := b.(type) {
		case string:
			if v != "" {
				out = append(out, map[string]any{"type": "text", "text": v})
			}
		case map[string]any:
			switch v["type"] {
			case "text", "":
				if t, ok := v["text"].(string); ok && t != "" {
					out = append(out, map[string]any{"type": "text", "text": t})
				}
			case "image_url":
				if img := imageBlock(v); img != nil {
					out = append(out, img)
				}
			case "image":
				// 已是 Anthropic 形态（部分客户端直接透传），原样保留
				out = append(out, v)
			}
		}
	}
	return out
}

// assistantBlocks 转换 assistant 消息：正文 + tool_calls → text / tool_use 块。
func assistantBlocks(m map[string]any) []any {
	var out []any
	for _, b := range contentBlocks(m["content"]) {
		switch v := b.(type) {
		case string:
			if v != "" {
				out = append(out, map[string]any{"type": "text", "text": v})
			}
		case map[string]any:
			if v["type"] == "text" {
				if t, ok := v["text"].(string); ok && t != "" {
					out = append(out, map[string]any{"type": "text", "text": t})
				}
			}
		}
	}
	calls, _ := m["tool_calls"].([]any)
	for _, c := range calls {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := cm["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		id, _ := cm["id"].(string)
		if id == "" {
			id = "call_" + name
		}
		out = append(out, map[string]any{
			"type":  "tool_use",
			"id":    id,
			"name":  name,
			"input": parseArgs(fn["arguments"]),
		})
	}
	return out
}

// toolResultBlocks 把 OpenAI 的 role=tool 消息转成 Anthropic 的 tool_result 块
// （Anthropic 里 tool_result 属于 user 消息）。
func toolResultBlocks(m map[string]any) []any {
	id, _ := m["tool_call_id"].(string)
	if id == "" {
		id, _ = m["name"].(string)
	}
	if id == "" {
		id = "call_unknown"
	}
	text := ""
	switch v := m["content"].(type) {
	case string:
		text = v
	case []any:
		var sb strings.Builder
		for _, b := range v {
			if bm, ok := b.(map[string]any); ok {
				if t, ok := bm["text"].(string); ok {
					sb.WriteString(t)
				}
			}
		}
		text = sb.String()
	}
	return []any{map[string]any{
		"type":        "tool_result",
		"tool_use_id": id,
		"content":     text,
	}}
}

// imageBlock 把 OpenAI 的 image_url 块转成 Anthropic 的 image 块。
// 支持 data: URL（base64）与 http(s) URL；其余形态丢弃（宁缺勿错）。
func imageBlock(v map[string]any) map[string]any {
	u, _ := v["image_url"].(map[string]any)
	url, _ := u["url"].(string)
	if url == "" {
		if s, ok := v["image_url"].(string); ok {
			url = s
		}
	}
	if url == "" {
		return nil
	}
	if strings.HasPrefix(url, "data:") {
		rest := strings.TrimPrefix(url, "data:")
		media, b64, ok := strings.Cut(rest, ";base64,")
		if !ok || media == "" || b64 == "" {
			return nil
		}
		return map[string]any{"type": "image", "source": map[string]any{
			"type": "base64", "media_type": media, "data": b64,
		}}
	}
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		return map[string]any{"type": "image", "source": map[string]any{
			"type": "url", "url": url,
		}}
	}
	return nil
}

// contentBlocks 把 content 归一化成块数组（字符串 → 单元素数组）。
func contentBlocks(v any) []any {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		if t == "" {
			return nil
		}
		return []any{t}
	case []any:
		return t
	default:
		return nil
	}
}

// textBlocks 只取文本块（用于 system 内容抽取）。
func textBlocks(v any) []any {
	switch t := v.(type) {
	case string:
		return []any{map[string]any{"type": "text", "text": t}}
	case []any:
		return t
	default:
		return nil
	}
}

// parseArgs 把 OpenAI 的工具入参（JSON 字符串）解析成对象；失败时回空对象
// （Anthropic 要求 input 必须是对象，给字符串会被拒）。
func parseArgs(v any) map[string]any {
	switch t := v.(type) {
	case map[string]any:
		return t
	case string:
		if strings.TrimSpace(t) == "" {
			return map[string]any{}
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(t), &out); err == nil && out != nil {
			return out
		}
	}
	return map[string]any{}
}

// convertTools 把 OpenAI 的 tools 定义转成 Anthropic 形态。
func convertTools(raw any) []any {
	list, _ := raw.([]any)
	var out []any
	for _, item := range list {
		t, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := t["function"].(map[string]any)
		if !ok {
			continue
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		tool := map[string]any{"name": name}
		if d, ok := fn["description"].(string); ok && d != "" {
			tool["description"] = d
		}
		if p, ok := fn["parameters"].(map[string]any); ok && len(p) > 0 {
			tool["input_schema"] = p
		} else {
			tool["input_schema"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, tool)
	}
	return out
}

// convertToolChoice 把 OpenAI 的 tool_choice 转成 Anthropic 形态。
// 无显式约束时返回 nil（不发送，交给上游默认 auto）。
func convertToolChoice(v any) map[string]any {
	switch t := v.(type) {
	case string:
		switch t {
		case "auto":
			return map[string]any{"type": "auto"}
		case "required", "any":
			return map[string]any{"type": "any"}
		case "none":
			// Anthropic 无 none 语义；此处返回 nil，由 toolsDisabled 在调用方
			// 裁掉整个 tools 字段来落实「不调用工具」。
			return nil
		}
	case map[string]any:
		if fn, ok := t["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok && name != "" {
				return map[string]any{"type": "tool", "name": name}
			}
		}
	}
	return nil
}
