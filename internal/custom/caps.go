package custom

// 上游 /models 能力探测（尽力解析，绝不编造）。
//
// 背景（R66 的补强）：自定义渠道的定位已从「兜底转发」变成「通用模型 api-key 聚合/管理」，
// 用户期望填完 base_url + api_key 就能直接勾选模型，而不是手抄一遍模型清单再瞎猜能力。
// 但 OpenAI 兼容的 /models 响应形态各家差得很远：
//   - OpenAI 官方：只有 id/object/created/owned_by —— 完全没有能力字段；
//   - OpenRouter 系：architecture.input_modalities、supported_parameters、context_length、
//     max_completion_tokens、supported_reasoning_levels；
//   - 各家自建网关（one-api/new-api/sgproxy/CLIProxy）：有的照抄官方，有的补
//     context_window/max_tokens/supports_* 这类扩展字段，还有的把扩展塞进 properties/config。
//
// 因此这里只做**宽容的字段别名提取**：命中就记，命中不了就留零值，并把来源如实标出来。
// 与 kilo/free.go 同一纪律（只认显式声明）；与 R49「思考档位只透传上游声明，绝不编造」
// 同源——探测不到 ≠ 不支持，也 ≠ 支持，必须由用户自己填。

// 能力来源标记（写入 ModelConfig.CapSource，面板据此显示，避免把猜测冒充上游声明）。
const (
	// CapSourceAPI 能力/上下文字段来自上游 /models 的显式声明。
	CapSourceAPI = "api"
	// CapSourceUser 用户在面板手填。
	CapSourceUser = "user"
	// CapSourceUnknown 既没探测到也没填 —— 前端显示「未探测」，不得显示具体数字或能力图标。
	CapSourceUnknown = "unknown"
)

// ModelCap 一条上游模型 + 尽力解析出的能力。
//
// 与 ModelConfig 的能力字段刻意使用同名 json，前端拿到即可直接写进渠道配置的 models 项；
// 探测结果只是「候选」，要不要写入配置由用户勾选决定（不自动添加，避免把上游的
// 全部模型一次性灌进池子）。
type ModelCap struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name,omitempty"` // 上游给的展示名（name/title），无则空
	// 上下文字段为 0 = 上游未声明（不是「不支持」，也不是「无限」）。
	ContextWindow     int64 `json:"context_window,omitempty"`
	MaxTokens         int64 `json:"max_tokens,omitempty"`
	SupportsImages    bool  `json:"supports_images,omitempty"`
	SupportsReasoning bool  `json:"supports_reasoning,omitempty"`
	// SupportsTools nil = 上游未声明。注意与 ModelConfig 的「nil ⇒ 默认支持工具」口径不同：
	// 这里是**探测结果的三态**，由前端决定怎么写进配置。
	SupportsTools          *bool    `json:"supports_tools,omitempty"`
	ReasoningEfforts       []string `json:"reasoning_efforts,omitempty"`
	DefaultReasoningEffort string   `json:"default_reasoning_effort,omitempty"` // 上游声明的默认档位
	// CapSource 非空表示确实解析到了至少一项能力/上下文字段（值恒为 CapSourceAPI）；
	// 空串 = 上游只给了 id，前端据此标「未探测」。
	CapSource string `json:"cap_source,omitempty"`
}

// hasAnyCap 报告是否解析到任一能力/容量字段。
func (m ModelCap) hasAnyCap() bool {
	return m.ContextWindow > 0 || m.MaxTokens > 0 || m.SupportsImages || m.SupportsReasoning ||
		m.SupportsTools != nil || len(m.ReasoningEfforts) > 0 || m.DefaultReasoningEffort != ""
}

// finalize 由解析器调用：解析到能力就回填 CapSource，供前端区分「上游声明」与「未探测」。
func (m *ModelCap) finalize() {
	if m.hasAnyCap() {
		m.CapSource = CapSourceAPI
	}
}

// ToModelConfig 把探测结果转成可直接落进渠道配置的一条模型。
//
// 转换只搬运、不补默认值：上游没声明的字段保持零值，来源标记原样带过去
// （未探测的条目转出来后 CapSource 仍是空，Validate() 会归成 CapSourceUnknown）。
func (m ModelCap) ToModelConfig() ModelConfig {
	mc := ModelConfig{
		ID:                     m.ID,
		ContextWindow:          m.ContextWindow,
		MaxTokens:              m.MaxTokens,
		SupportsImages:         m.SupportsImages,
		SupportsReasoning:      m.SupportsReasoning,
		SupportsTools:          m.SupportsTools,
		ReasoningEfforts:       m.ReasoningEfforts,
		DefaultReasoningEffort: m.DefaultReasoningEffort,
		CapSource:              m.CapSource,
	}
	return mc
}
