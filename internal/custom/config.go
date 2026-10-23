// Package custom 实现自定义 OpenAI 兼容第三方渠道（R66）。
//
// 目标：允许用户在面板配置任意 OpenAI 兼容上游（如官方 StepFun、本地 CLIProxy、
// DeepSeek、SiliconFlow 等），由本工具统一进行模型名前缀路由（channel/model），
// 统一记录用量流水（ledger）与运行统计（stats，含 TTFB、耗时、吞吐与请求日志）。
//
// 原则（用户裁定）：
//   - 配什么，/v1/models 就返回什么；
//   - 运行期路由绝不向上游 GET /models 发起拉取（只有面板上用户主动点「导入」
//     才探一次，见 probe.go 的 TestConnection）；
//   - 绝不做任何启发式推测：能力字段只记「谁说的」（CapSource），上游没声明、
//     用户也没填的一律留「未探测」，不得拿默认值冒充声明。
package custom

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"wild-work/internal/provider"
)

var channelNameRegex = regexp.MustCompile(`^[a-zA-Z0-9_\-]+$`)

// ModelConfig 单个模型详细配置（配什么返回什么，运行期不推测）。
type ModelConfig struct {
	ID                     string   `json:"id"`
	ContextWindow          int64    `json:"context_window,omitempty"`           // 上下文大小（tokens），0 = 未设置
	MaxTokens              int64    `json:"max_tokens,omitempty"`               // 最大输出 tokens，0 = 未设置
	SupportsImages         bool     `json:"supports_images,omitempty"`          // 是否支持视觉/多模态
	SupportsReasoning      bool     `json:"supports_reasoning,omitempty"`       // 是否支持思考/推理
	SupportsTools          *bool    `json:"supports_tools,omitempty"`           // 工具/函数调用（nil = 未声明 ⇒ 默认 true；指针以保留显式 false，否则 omitempty 会把 false 丢掉）
	ReasoningEfforts       []string `json:"reasoning_efforts,omitempty"`        // 思考档位，如 ["low", "medium", "high"]
	DefaultReasoningEffort string   `json:"default_reasoning_effort,omitempty"` // 默认思考档位，如 "high"
	// CapSource 上面这些能力字段的**来源**："api"=上游 /models 声明、"user"=面板手填、
	// ""/"unknown"=未探测。为什么要落盘：导入时把「1M/64k/全能力」这类猜测值当声明写死，
	// 面板与 /v1/models 就会把「不知道」冒充成「上游说的」（R49 同款纪律）。
	// 有来源才能如实标注，用户也才能只校正「未探测」的那些。
	// 零值 = 未探测 ⇒ 老配置（无此字段）行为不变，符合不变量 5（只增不减）。
	CapSource string `json:"cap_source,omitempty"`
}

// CapFromAPI 能力字段是否来自上游声明（面板据此标「上游」、/v1/models 据此置 ContextFromAPI）。
func (m ModelConfig) CapFromAPI() bool { return m.CapSource == CapSourceAPI }

// CapFromUser 能力字段是否用户在面板手填（面板据此标「手填」，与「上游」「实测」区分）。
func (m ModelConfig) CapFromUser() bool { return m.CapSource == CapSourceUser }

// HasAnyCap 是否有任何确定的能力/容量信息（含只填了上下文的情形）——
// 「未探测」的后端口径（前端目前直接看 cap_source，两者得一致）。
// 注意：SupportsTools 只数「显式 false」——解析层给缺省也补 true（简写形态不会落成
// 「不支持工具」，见 UnmarshalJSON 注释），若把 nil/true 也算「已探测」，
// `"models":["id"]` 这种只填了 ID 的老配置会被当成有能力信息。
func (m ModelConfig) HasAnyCap() bool {
	return m.ContextWindow > 0 || m.MaxTokens > 0 || m.SupportsImages || m.SupportsReasoning ||
		(m.SupportsTools != nil && !*m.SupportsTools) || len(m.ReasoningEfforts) > 0 || m.DefaultReasoningEffort != ""
}

// boolPtr 返回 bool 的指针（用于区分三态：nil = 未声明，取默认）。
func boolPtr(b bool) *bool { return &b }

// ToolsEnabled 报告该模型是否支持工具调用：未声明（nil）视为支持，与旧配置向后兼容。
func (m ModelConfig) ToolsEnabled() bool { return m.SupportsTools == nil || *m.SupportsTools }

// ParseTokens 解析带 k/K/m/M 单位的 token 数量（如 1M, 64k, 128k, 1000000）。
func ParseTokens(v any) (int64, error) {
	switch val := v.(type) {
	case float64:
		return int64(val), nil
	case int64:
		return val, nil
	case int:
		return int64(val), nil
	case string:
		s := strings.TrimSpace(strings.ToUpper(val))
		if s == "" {
			return 0, nil
		}
		multiplier := int64(1)
		if strings.HasSuffix(s, "M") {
			multiplier = 1000000
			s = strings.TrimSuffix(s, "M")
		} else if strings.HasSuffix(s, "K") {
			multiplier = 1000
			s = strings.TrimSuffix(s, "K")
		}
		s = strings.TrimSpace(s)
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid token amount %q: %w", val, err)
		}
		return int64(f * float64(multiplier)), nil
	default:
		return 0, fmt.Errorf("unsupported token type %T", v)
	}
}

// UnmarshalJSON 兼容简写字符串（如 "step-5-preview"）与完整 JSON 对象
// （context_window / max_tokens 支持 1M / 64k 带单位串）。
//
// 注意：**字符串形态也要补 SupportsTools=true**（缺省 = 支持）。新字段一律以
// 「零值 = 关闭」声明（bool 无三态），故默认值必须在每条解析路径上显式补上——
// 只在对象分支补会让 `["step-5-preview"]` 这种最常用的简写落成「不支持工具」，
// 静默地从 /v1/models 消失（客户端据此判断能否发 tools）。
func (m *ModelConfig) UnmarshalJSON(data []byte) error {
	// 默认值先置位，两条分支都不必重复（与 SupportsImages/Reasoning 不同：那两个
	// 上游默认就是 false，这里默认是 true）。
	m.SupportsTools = boolPtr(true)
	data = bytes.TrimSpace(data)
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		m.ID = strings.TrimSpace(s)
		return nil
	}
	var raw struct {
		ID                     string          `json:"id"`
		ContextWindow          json.RawMessage `json:"context_window,omitempty"`
		MaxTokens              json.RawMessage `json:"max_tokens,omitempty"`
		SupportsImages         bool            `json:"supports_images,omitempty"`
		SupportsReasoning      bool            `json:"supports_reasoning,omitempty"`
		SupportsTools          *bool           `json:"supports_tools,omitempty"`
		ReasoningEfforts       []string        `json:"reasoning_efforts,omitempty"`
		DefaultReasoningEffort string          `json:"default_reasoning_effort,omitempty"`
		CapSource              string          `json:"cap_source,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.ID = strings.TrimSpace(raw.ID)
	m.SupportsImages = raw.SupportsImages
	m.SupportsReasoning = raw.SupportsReasoning
	m.SupportsTools = raw.SupportsTools
	m.ReasoningEfforts = raw.ReasoningEfforts
	m.DefaultReasoningEffort = raw.DefaultReasoningEffort
	m.CapSource = strings.TrimSpace(raw.CapSource)

	// 单位串解析失败不吞错：把错误往上抛，让配置层 normalize() 报「哪一条渠道的哪个字段
	// 写错了」，而不是静默落成 0（= 未设置）——否则用户输错单位后会看到「上下文未知」
	// 却无人告知，属 R55 的「展示字段不得吞掉配置错误」同类。
	if len(raw.ContextWindow) > 0 {
		v, err := decodeTokenAmount(raw.ContextWindow)
		if err != nil {
			return fmt.Errorf("context_window: %w", err)
		}
		m.ContextWindow = v
	}
	if len(raw.MaxTokens) > 0 {
		v, err := decodeTokenAmount(raw.MaxTokens)
		if err != nil {
			return fmt.Errorf("max_tokens: %w", err)
		}
		m.MaxTokens = v
	}
	return nil
}

// decodeTokenAmount 把 JSON 原始值（数字或带单位串）解成 token 数。
func decodeTokenAmount(raw json.RawMessage) (int64, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, err
	}
	return ParseTokens(v)
}

// ChannelConfig 自定义 OpenAI 兼容渠道配置。
type ChannelConfig struct {
	Name    string        `json:"name"`              // 渠道标识/模型前缀，如 "stepfun", "proxycli", "cliproxy"
	BaseURL string        `json:"base_url"`          // API 基础地址，如 "https://api.stepfun.com/step_plan/v1"
	APIKey  string        `json:"api_key,omitempty"` // API 密钥（可选）
	Models  []ModelConfig `json:"models,omitempty"`  // 用户配置的模型列表（配什么，/v1/models 就返回什么）
	// R83 起移除 `enabled` 字段：渠道要么在列表里（启用），要么被删除，
	// 「停用但保留配置」这个中间态既少用又让面板多一个按钮和一层视觉噪音。
	// 旧配置里残留的 enabled 字段会被 JSON 解码静默忽略（不影响加载）。
}

// Validate 校验配置合法性并规整字段。
func (c *ChannelConfig) Validate() error {
	c.Name = strings.TrimSpace(strings.ToLower(c.Name))
	// 接入点走统一规范化（R121）：缺协议头要补、?query/#fragment 要丢、结尾的
	// /chat/completions 尾巴要剥。手改 config.json 也会经过这里（config.Normalize 逐渠道调 Validate）。
	c.BaseURL = NormalizeBaseURL(c.BaseURL)
	c.APIKey = strings.TrimSpace(c.APIKey)

	if c.Name == "" {
		return fmt.Errorf("channel name cannot be empty")
	}
	if !channelNameRegex.MatchString(c.Name) {
		return fmt.Errorf("channel name %q contains invalid characters (only letters, numbers, _, - allowed)", c.Name)
	}
	// 拒绝内建渠道名与上游命名空间（R66）：同名会让 server/app 两层指向不同 Runtime，
	// 静默出错（面板操作打到自定义池、对话请求走内建渠道）。
	if provider.BuiltinKinds()[c.Name] {
		return fmt.Errorf("channel name %q 与内建渠道（或上游命名空间）同名，请另取一个标识", c.Name)
	}
	if c.BaseURL == "" {
		return fmt.Errorf("channel %s: base_url cannot be empty", c.Name)
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("channel %s: invalid base_url %q (must be http:// or https://)", c.Name, c.BaseURL)
	}

	var cleanModels []ModelConfig
	seen := map[string]bool{}
	for _, m := range c.Models {
		m.ID = strings.TrimSpace(m.ID)
		// 来源字段只接受三个枚举值，缺失/写错的一律归「未探测」（不得因为脏数据而谎称「上游声明」）。
		// 简写形态（models:["id"]）本就没得源可标，同样落这里 ⇒ 前端统一看到「未探测」。
		switch m.CapSource {
		case CapSourceAPI, CapSourceUser:
		default:
			m.CapSource = CapSourceUnknown
		}
		if m.ID != "" && !seen[m.ID] {
			seen[m.ID] = true
			cleanModels = append(cleanModels, m)
		}
	}
	c.Models = cleanModels
	return nil
}
