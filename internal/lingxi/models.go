package lingxi

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// plansResponse 是 GET /api/aioffice/v1/sessions/plans 的响应信封。
type plansResponse struct {
	Result string     `json:"result"`
	V7Code int        `json:"v7code"`
	Data   plansData  `json:"data"`
}

type plansData struct {
	// plans（plan_pro/plan_max 套餐包）不解析、不展示：本渠道按对话扣智点，
	// 套餐信息面板用不上（2026-10-29 审查 S-5 死代码清理，原结构体无消费方）。
	Models []modelEntry `json:"models"`
}

type modelEntry struct {
	Key              string   `json:"key"`
	Name             string   `json:"name"`
	Desc             string   `json:"desc"`
	Tag              string   `json:"tag"`
	Multiplier       string   `json:"multiplier"`
	SupportMultimodal bool    `json:"support_multimodal"`
	MaxContextTokens int64    `json:"max_context_tokens"`
	MaxNewTokens     int64    `json:"max_new_tokens"`
	Intro            string   `json:"intro"`
	ReasoningEfforts []string `json:"reasoning_efforts"`
}

// FetchModels 实现 provider.Upstream。
//
// 拉取成功→更新 knownIDs 缓存并返回目录；拉取失败→回退上次已知目录（R42
// fail-open：目录端点偶发抖动不该让渠道从面板上消失）。**冷启动**（缓存为空）
// 拉取失败时返回错误，但 ChatStream 的 knownModel 校验对空缓存放行（见
// client.go），确保「面板模型列表空」≠「对话请求被拦」。
func (c *Client) FetchModels(a *auth.Auth) ([]provider.ModelInfo, error) {
	raw, err := c.getJSON(a, EpPlans)
	if err != nil {
		// 缓存兜底。
		c.knownMu.RLock()
		cached := make([]provider.ModelInfo, 0, len(c.knownIDs))
		for id, name := range c.knownIDs {
			cached = append(cached, provider.ModelInfo{ID: id, Name: name})
		}
		c.knownMu.RUnlock()
		if len(cached) > 0 {
			sort.Slice(cached, func(i, j int) bool { return cached[i].ID < cached[j].ID })
			return cached, nil
		}
		return nil, err
	}

	var resp plansResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("lingxi: plans 响应解析失败: %w", err)
	}
	if resp.Result == "UserUnauthorized" {
		return nil, fmt.Errorf("lingxi: 凭据失效（UserUnauthorized）")
	}

	known := make(map[string]string, len(resp.Data.Models))
	infos := make([]provider.ModelInfo, 0, len(resp.Data.Models))
	for _, m := range resp.Data.Models {
		if strings.TrimSpace(m.Key) == "" {
			continue
		}
		known[m.Key] = m.Name
		info := provider.ModelInfo{
			ID:            m.Key,
			Name:          m.Name,
			ContextWindow: m.MaxContextTokens,
			MaxTokens:     m.MaxNewTokens,
			ContextFromAPI: m.MaxContextTokens > 0,
		}
		if m.SupportMultimodal {
			info.SupportsImages = true
		}
		if len(m.ReasoningEfforts) > 0 {
			info.SupportsReasoning = true
			info.ReasoningEfforts = append([]string(nil), m.ReasoningEfforts...)
			// 面板默认档：三档取中间档，两档取低档（保守——高档推理更烧智点）。
			// 2026-10-29 审查发现D 修复：原 [len/2] 对两档取 [1]（高档），
			// 与注释所述「两档的低」矛盾且费智点；(len-1)/2 使 2 档→[0] 低、3 档→[1] 中。
			if len(info.ReasoningEfforts) >= 2 {
				info.DefaultReasoningEffort = info.ReasoningEfforts[(len(info.ReasoningEfforts)-1)/2]
			} else {
				info.DefaultReasoningEffort = info.ReasoningEfforts[0]
			}
		}
		if m.Multiplier != "" {
			info.Name = fmt.Sprintf("%s（%s）", m.Name, m.Multiplier)
		}
		infos = append(infos, info)
	}

	c.knownMu.Lock()
	c.knownIDs = known
	c.knownMu.Unlock()

	if len(infos) == 0 {
		return nil, fmt.Errorf("lingxi: plans 返回 0 模型")
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].ID < infos[j].ID })
	return infos, nil
}

// knownModel 本地模型校验（ChatStream 前置）。
// 缓存为空→放行（fail-open，R42）；缓存非空且不含→拦（404）。
func (c *Client) knownModel(m string) bool {
	c.knownMu.RLock()
	defer c.knownMu.RUnlock()
	if len(c.knownIDs) == 0 {
		return true
	}
	_, ok := c.knownIDs[m]
	return ok
}

// StaticModels 恒 nil：本渠道不做本地假设（无静态表、无映射、无过滤），
// 模型面完全由上游 plans 的 data.models 决定，原样透传（与 comate 同款）。
// 兜底语义见 handler.CachedChannelModels —— 缓存优先于静态表。
func StaticModels() []provider.ModelInfo { return nil }
