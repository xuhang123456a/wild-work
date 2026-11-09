package comate

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// ---------------------------------------------------------------------------
// 动态目录（唯一来源）
// ---------------------------------------------------------------------------

// remoteModel 是 /models 响应 data.items 数组元素的形状（字段按需取用）。
type remoteModel struct {
	DisplayName     string   `json:"display_name"`
	Name            string   `json:"name"` // 上游完整 ID，如 673940726/zhipu/glm-5.3-flash//public
	Description     string   `json:"description"`
	LLMTypes        []string `json:"llm_types"` // llm-chat / llm-multimodal
	ContextWindow   int64    `json:"context_window"`
	SupportThinking bool     `json:"support_thinking"`
	ThinkingLevels  []string `json:"thinking_levels"`
	ModelTier       string   `json:"model_tier"`
	ConsumeRatio    float64  `json:"consume_ratio"`
}

// modelsResp /models 信封：{"code":0,"msg":"","data":{"auto":{…},"items":[…]}}。
type modelsResp struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Auto  json.RawMessage `json:"auto"`
		Items []remoteModel   `json:"items"`
	} `json:"data"`
}

// StaticModels 恒 nil：本渠道**不做任何本地假设**（无静态表、无映射、无过滤）。
// 模型面完全由上游 /models 的 items 数组决定，原样透传。
// 兜底语义见 handler.CachedChannelModels —— 缓存优先于静态表，空表不会让渠道分组消失
//（与 cline/qodercn/qodercom 同款先例）。
func StaticModels() []provider.ModelInfo { return nil }

// FetchModels 返回暴露集 = /models 的 items 数组**原样透传**：
//  1. ID 不改名不改前缀（items[].name 就是上游完整模型 ID，含 //public 尾巴）；
//  2. 不按 tier/llm_types 过滤 —— 上游给的什么就暴露什么；
//  3. 仅做两条机械兜底：精确 ID 去重、name 缺失时用 prettifyID 兜底；
//  4. context_window / support_thinking / llm_types 上游**有**权威声明
//     （与 cline 不同，无需本地实测表），直接填入 ModelInfo，ContextFromAPI=true。
//
// 上游目录瞬时抖动时返回缓存（knownIDs），不掏空面板。
func (c *Client) FetchModels(a *auth.Auth) ([]provider.ModelInfo, error) {
	raw, err := c.getJSON(a, EpModels)
	var rec modelsResp
	if err == nil {
		err = json.Unmarshal(raw, &rec)
	}
	if err == nil && rec.Code != 0 {
		err = fmt.Errorf("comate: /models 返回 code=%d msg=%s", rec.Code, rec.Msg)
	}
	if err != nil {
		// 兜底：items 为空但缓存尚有内容 ⇒ 返回缓存（上游目录瞬时抖动不掏空面板）
		c.knownMu.RLock()
		cached := make([]provider.ModelInfo, 0, len(c.knownIDs))
		for id, name := range c.knownIDs {
			cached = append(cached, modelInfoOf(id, name))
		}
		c.knownMu.RUnlock()
		sort.Slice(cached, func(i, j int) bool { return cached[i].ID < cached[j].ID })
		if len(cached) > 0 {
			return cached, nil
		}
		return nil, fmt.Errorf("comate: 模型目录拉取失败: %w", err)
	}

	// 空目录不是「成功」：真实账号可能因额度/开通状态被上游下发 items=[]，
	// 而 handler 的模型缓存是被单个账号 Pick 出来的 —— 一旦这次 Pick 到空账号，
	// 整个渠道的模型列表会瞬间归零（实测：加一个未开通 Comate 的账号后 9→0）。
	// 所以空目录一律回落到已知缓存，绝不覆盖面板。
	if len(rec.Data.Items) == 0 {
		c.knownMu.RLock()
		cached := make([]provider.ModelInfo, 0, len(c.knownIDs))
		for id, name := range c.knownIDs {
			cached = append(cached, modelInfoOf(id, name))
		}
		c.knownMu.RUnlock()
		sort.Slice(cached, func(i, j int) bool { return cached[i].ID < cached[j].ID })
		if len(cached) > 0 {
			return cached, nil
		}
		return nil, fmt.Errorf("comate: 该账号没有可用模型（/models 返回空目录，通常是 Comate 未开通或额度为 0）")
	}

	seen := map[string]bool{}
	out := make([]provider.ModelInfo, 0, len(rec.Data.Items))
	for _, m := range rec.Data.Items {
		id := strings.TrimSpace(m.Name)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		name := strings.TrimSpace(m.DisplayName)
		if name == "" || name == id {
			name = prettifyID(id)
		}
		out = append(out, enrichModelInfo(modelInfoOf(id, name), m))
		c.rememberLiveModel(id, name)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// FetchModelPricing 实现 provider.Upstream：把 /models 的 consume_ratio 当倍率上报
// （R136「把 model 补上」的一半——模型清单早已动态透传，缺的是每款的消耗倍率）。
//
// 上游事实：每款模型带 `consume_ratio`（实测 0.04 ~ 0.75），它就是「按 token 计费时
// 每 token 消耗的积分系数」——usage.credits 与它对得上（≈ consume_ratio × tokens / 1000
// 量级），故可作为面板「倍率」列的真值，Explicit=true（不是猜的）。
//
// consume_ratio 为 0/缺失时不猜：Rate=0 + Explicit=false，面板显示「未知」而不是
// 「免费」（R5 约定；loomy 曾因共享同一个 explicit 指针让这条约定静默失效，见其注释）。
func (c *Client) FetchModelPricing(a *auth.Auth) ([]provider.ModelPricing, error) {
	raw, err := c.getJSON(a, EpModels)
	if err != nil {
		return nil, err
	}
	var rec modelsResp
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("comate: /models 响应解析失败: %w", err)
	}
	if rec.Code != 0 {
		return nil, fmt.Errorf("comate: /models 返回 code=%d msg=%s", rec.Code, rec.Msg)
	}
	out := make([]provider.ModelPricing, 0, len(rec.Data.Items))
	seen := map[string]bool{}
	for _, m := range rec.Data.Items {
		id := strings.TrimSpace(m.Name)
		if id == "" || seen[id] || m.ConsumeRatio <= 0 {
			continue
		}
		seen[id] = true
		// 每条独立取址：共享同一个 bool 会让所有条目的 Explicit 恒等于末值。
		explicit := true
		out = append(out, provider.ModelPricing{
			Model:    id,
			Channel:  string(Kind),
			Rate:     m.ConsumeRatio,
			Note:     strings.TrimSpace(m.ModelTier),
			Explicit: &explicit,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out, nil
}

// modelInfoOf 组装单条模型信息：基础字段来自上游 items[]。
// 上下文/多模态/思考能力上游**有**权威声明（ContextFromAPI=true），
// 但本函数只拿 id/name —— 能力字段在 FetchModels 里按 remoteModel 填充。
//
// 注：knownIDs 缓存路径（目录拉取失败时的兜底）只有 id/name 可用，
// 能力字段缺失是可接受的降级（面板显示「未知」而非虚报）。
func modelInfoOf(id, name string) provider.ModelInfo {
	return provider.ModelInfo{ID: id, Name: name}
}

// enrichModelInfo 把 remoteModel 的能力声明填进 ModelInfo（ContextFromAPI=true）。
func enrichModelInfo(mi provider.ModelInfo, m remoteModel) provider.ModelInfo {
	if m.ContextWindow > 0 {
		mi.ContextWindow = m.ContextWindow
		mi.ContextFromAPI = true
	}
	for _, t := range m.LLMTypes {
		if t == "llm-multimodal" {
			mi.SupportsImages = true
		}
	}
	if m.SupportThinking {
		mi.SupportsReasoning = true
		mi.ContextFromAPI = true
	}
	if len(m.ThinkingLevels) > 0 {
		mi.ReasoningEfforts = append([]string(nil), m.ThinkingLevels...)
	}
	return mi
}

// prettifyID 把上游完整 ID 变成可读展示名（仅在上游未给 display_name 时兜底）。
// 形态：673940726/zhipu/glm-5.3-flash//public → GLM-5.3 Flash。
//
// ⚠️ 单段 ID（无 "/"，如 glm-5.3-flash）必须有兜底分支：旧实现直接取
// `parts[len(parts)-2]` ⇒ 越界 panic（index out of range [-1]），而上游
// display_name 为空时这条路径是**必走**的 ⇒ 整个 FetchModels 崩。
func prettifyID(id string) string {
	s := id
	if parts := strings.Split(id, "/"); len(parts) >= 2 {
		// 末段是可见性标记（//public、/private），先剔掉；再取剩下最后一个非空段。
		if last := strings.ToLower(strings.TrimSpace(parts[len(parts)-1])); last == "public" || last == "private" {
			parts = parts[:len(parts)-1]
		}
		s = ""
		for i := len(parts) - 1; i >= 0; i-- {
			if p := strings.TrimSpace(parts[i]); p != "" {
				s = p
				break
			}
		}
	}
	s = strings.ReplaceAll(strings.TrimSpace(s), "-", " ")
	if r := []rune(s); len(r) > 0 {
		return strings.ToUpper(string(r[0])) + string(r[1:])
	}
	return s
}

// ---------------------------------------------------------------------------
// 校验集
// ---------------------------------------------------------------------------

// knownModel 本地校验。**冷启动放行**：目录尚未成功拉取过（knownIDs 为空）时不拦截 ——
// 若拦，daemon 重启后第一批请求会全部被本地 404（等于「渠道周期性瘫痪」），
// 而放行的最坏结果只是让上游给出权威错误（R42 的教训）。
func (c *Client) knownModel(id string) bool {
	c.knownMu.RLock()
	defer c.knownMu.RUnlock()
	if len(c.knownIDs) == 0 {
		return true
	}
	_, ok := c.knownIDs[id]
	return ok
}

// rememberLiveModel 把 items[] 条目记入已知集（ID → name，**替换式**更新）。
func (c *Client) rememberLiveModel(id, name string) {
	c.knownMu.Lock()
	defer c.knownMu.Unlock()
	if c.knownIDs == nil {
		c.knownIDs = map[string]string{}
	}
	c.knownIDs[id] = name
}
