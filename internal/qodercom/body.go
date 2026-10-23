// body.go 构造 agent_chat_generation 请求体。
// 以 qoder2api baseprompt.json 模板为准（区别于 qoderwork 渠道的精简体）：
//   - session_type: "qoder"（qoderwork 是 "qodercli"）
//   - 额外字段：is_retry/code_language/source/version/chat_prompt/parameters/task_id
//   - model_config 全字段（key/display_name/format/is_vl/api_key/url/source/max_input_tokens）
//   - aliyun_user_type 用账号实测 userType
package qodercom

import (
	"encoding/json"
	"time"
)

// buildAgentBody 构造请求体。
//   - messages：客户端原始消息列表（可含 system/assistant/tool 多轮）
//   - mc：model_config 条目（来自动态模型表；nil 时用 auto 兜底）
//   - tools：客户端传来的 OpenAI tools 数组；为空则不注入 tools 字段
//   - enableReasoning：是否启用思考模式
//   - effort：客户端选定的思考档位（low/high/max 等）；空 = 未指定，不注入
//   - maxTokens：客户端请求的 max_tokens，<=0 时用模板默认 32768
//   - contextWindow：选定的上下文档位（200K/400K/1M）；<=0 时不注入 context_length
//
// 注意：developer 角色必须改写为 system。
func buildAgentBody(messages []map[string]any, mc *ModelEntry, tools []any, enableReasoning bool, effort string, maxTokens int, userType string, contextWindow int64) ([]byte, error) {
	// developer → system（浅拷贝消息避免污染调用方数据）
	msgs := make([]map[string]any, len(messages))
	for i, m := range messages {
		cp := make(map[string]any, len(m)+1)
		for k, v := range m {
			cp[k] = v
		}
		if r, _ := cp["role"].(string); r == "developer" {
			cp["role"] = "system"
		}
		msgs[i] = cp
	}

	// 最后一条 user 消息文本（chat_context.text 上游协议要求必填）
	prompt := ""
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i]["role"] == "user" {
			if c, ok := msgs[i]["content"].(string); ok && c != "" {
				prompt = c
				break
			}
		}
	}

	if maxTokens <= 0 {
		maxTokens = 32768 // qoder2api baseprompt.json parameters.max_tokens 默认
	}
	if userType == "" {
		userType = "personal_standard"
	}
	modelCfg := modelConfigFrom(mc, enableReasoning)

	// 上下文档位透传（issue #27）：对齐 9router——parameters.context_length + model_config.max_input_tokens。
	params := map[string]any{"max_tokens": maxTokens}
	if contextWindow > 0 {
		params["context_length"] = contextWindow
		modelCfg["max_input_tokens"] = contextWindow
	}
	// 思考档位透传（对齐官方客户端与 EchoPing07/Qoder-2API-Go 的 verbatim 策略）：
	// 上游网关认 parameters.reasoning_effort，档位取值来自 thinking_config.enabled.efforts。
	// 空串=客户端未指定，不注入（让上游走自己声明的 is_default 档）。
	// effort=="none" 是关闭思考的开关，与官方客户端一致：同时置 max_thinking_tokens=0 与 is_reasoning=false。
	if effort != "" {
		params["reasoning_effort"] = effort
		if effort == "none" {
			params["max_thinking_tokens"] = 0
			enableReasoning = false
			modelCfg["is_reasoning"] = false
		} else {
			// 显式非 none 档位强制开启思考（覆盖上游 is_reasoning 与 thinking_config 矛盾的情形）。
			enableReasoning = true
			modelCfg["is_reasoning"] = true
		}
	}

	now := time.Now()
	newUUID := uuid4()

	base := map[string]any{
		"request_id":       newUUID,
		"chat_record_id":   newUUID,
		"request_set_id":   uuid4(),
		"session_id":       uuid4(),
		"stream":           true,
		"aliyun_user_type": userType,
		"agent_id":         "agent_common",
		"chat_task":        "FREE_INPUT",
		"is_reply":         true,
		// qoder2api baseprompt.json 模板独有字段
		"is_retry":      false,
		"code_language": "",
		"source":        1,
		"version":       "3",
		"chat_prompt":   "",
		"task_id":       "common",
		"parameters":    params,
		"session_type":  "qoder", // qoderwork 渠道是 "qodercli"
		"model_config":  modelCfg,
		"chat_context": map[string]any{
			"chatPrompt": "",
			"text":       map[string]any{"type": "text", "text": prompt},
			"extra": map[string]any{
				"context":         []any{},
				"modelConfig":     copyModelConfigLite(modelCfg),
				"originalContent": map[string]any{"type": "text", "text": prompt},
			},
			"features":  []any{},
			"imageUrls": nil,
		},
		"messages": msgs,
		"business": map[string]any{
			"id":       uuid4(),
			"begin_at": now.UnixMilli(),
			"name":     truncateRunes(prompt, 30),
		},
	}

	if len(tools) > 0 {
		base["tools"] = tools
	}

	return json.Marshal(base)
}

// ModelEntry 动态模型表条目（models.go 解析自上游）。
type ModelEntry struct {
	Key            string  `json:"key"`
	DisplayName    string  `json:"display_name"`
	Enable         bool    `json:"enable"`
	IsDefault      bool    `json:"is_default"`
	IsReasoning    bool    `json:"is_reasoning"`
	IsVL           bool    `json:"is_vl"`
	MaxInputTokens int64   `json:"max_input_tokens"`
	PriceFactor    float64 `json:"price_factor"`
	Format         string  `json:"format"` // 上游声明的协议形态（实测两渠道恒为 "openai"）
	Source         string  `json:"source"` // 上游声明来源；model_config.source 即思考总开关
	ContextWindow  int64   `json:"-"`      // context_config 默认档（is_default）的 token_count
	// AvailableWindows context_config 全部档位（升序）；空表示无档位表。
	AvailableWindows []int64 `json:"-"`

	// ReasoningEfforts thinking_config.enabled.efforts 的档位名（低→高排序）；空表示上游未声明档位。
	// DefaultReasoningEffort 是上游标 is_default 的那一档（不传档位时上游实际走的档位）。
	ReasoningEfforts       []string `json:"-"`
	DefaultReasoningEffort string   `json:"-"`
}

// 上游未下发时的兜底值（实测两渠道 204/204 条目均下发，此处仅防御）。
const (
	defaultFormat = "openai"
	defaultSource = "system"
)

// modelConfigFrom 构造 model_config（qoder2api baseprompt.json 全字段形态）。
func modelConfigFrom(m *ModelEntry, enableReasoning bool) map[string]any {
	if m == nil || m.Key == "" {
		return map[string]any{
			"key": "auto", "display_name": "Auto", "model": "", "format": defaultFormat,
			"is_vl": false, "is_reasoning": enableReasoning, "api_key": "", "url": "",
			"source": defaultSource, "max_input_tokens": 180000,
		}
	}
	format := m.Format
	if format == "" {
		format = defaultFormat // 上游偶发未下发时兜底
	}
	source := m.Source
	if source == "" {
		source = defaultSource
	}
	maxIn := m.MaxInputTokens
	if maxIn <= 0 {
		maxIn = 180000
	}
	return map[string]any{
		"key": m.Key, "display_name": m.DisplayName, "model": "", "format": format,
		"is_vl": m.IsVL, "is_reasoning": enableReasoning, "api_key": "", "url": "",
		"source": source, "max_input_tokens": maxIn,
	}
}

// copyModelConfigLite chat_context.extra.modelConfig 仅需 key/is_reasoning（模板形态）。
func copyModelConfigLite(mc map[string]any) map[string]any {
	return map[string]any{"key": mc["key"], "is_reasoning": mc["is_reasoning"]}
}

// resolveContextWindow 解析本次请求应使用的上下文档位。
// 客户端值校验对齐官方 TZ；默认档与非法值落点取 max(AvailableWindows)
// （宁高勿低，对齐 workbuddy2api 广告口径），而非官方 FHA 的 is_default：
//  1. requested>0 且校验通过 → 用 requested
//  2. 否则 → max(AvailableWindows)（有档位表时）
//  3. 否则 → ContextWindow → MaxInputTokens → 0（不注入）
func resolveContextWindow(requested int64, mc *ModelEntry) int64 {
	if requested > 0 {
		if mc == nil {
			return requested
		}
		if len(mc.AvailableWindows) > 0 {
			for _, w := range mc.AvailableWindows {
				if w == requested {
					return requested
				}
			}
			// 不在表内 → 落最大档（本仓默认）
		} else {
			if mc.MaxInputTokens <= 0 || requested <= mc.MaxInputTokens {
				return requested
			}
		}
	}
	if mc == nil {
		return 0
	}
	if n := len(mc.AvailableWindows); n > 0 {
		return mc.AvailableWindows[n-1] // 升序，取最大档
	}
	if mc.ContextWindow > 0 {
		return mc.ContextWindow
	}
	if mc.MaxInputTokens > 0 {
		return mc.MaxInputTokens
	}
	return 0
}

// parseContextWindowHint 从 OpenAI 请求体读取客户端上下文档位提示。
// 同时认 context_length（上游参数名）与 context_window（官方 SDK 驼峰名）。
func parseContextWindowHint(body []byte) int64 {
	var hint struct {
		ContextLength int64 `json:"context_length"`
		ContextWindow int64 `json:"context_window"`
	}
	if err := json.Unmarshal(body, &hint); err != nil {
		return 0
	}
	if hint.ContextLength > 0 {
		return hint.ContextLength
	}
	return hint.ContextWindow
}

// truncateRunes 截断到 n 个 rune。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
