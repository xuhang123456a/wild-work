package custom

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"wild-work/internal/provider"
)

// 上游 /models 里各家网关的能力字段别名表。
//
// 为什么要别名表而不是「就认 OpenAI 官方形状」：官方 /models 根本不返回能力，
// 而第三方网关大量私扩字段——用户想要的「能拿到能力就一并拉」只能靠宽容匹配实现。
// 取值为点分路径（支持嵌套对象），命中第一个非空即用；顺序 = 优先级
// （越靠前的越权威，避免 `properties` 里的猜测覆盖顶层的官方声明）。
var (
	ctxKeys    = []string{"context_window", "context_length", "max_input_tokens", "architecture.input_context", "top_provider.context_length", "properties.context_window"}
	outKeys    = []string{"max_tokens", "max_completion_tokens", "output_context", "architecture.output_context", "top_provider.max_completion_tokens", "properties.max_tokens"}
	imagesKeys = []string{"supports_images", "vision", "capabilities.image_input", "supports_vision"}
	toolsKeys  = []string{"supports_tools", "function_calling", "capabilities.tools"}
	rsnKeys    = []string{"supports_reasoning", "capabilities.reasoning"}
	effortKeys = []string{"reasoning_efforts", "supported_reasoning_levels", "supported_reasoning_efforts", "capabilities.reasoning_efforts"}
	// modality/参数列表类：值本身是数组或 `text+image->text` 这种拼接串，靠子串匹配。
	listKeys = []string{"supported_parameters", "supported_features", "capabilities", "architecture.input_modalities", "features", "tags"}
	defEff   = []string{"default_reasoning_effort", "reasoning.default_effort", "supported_reasoning_level"}
	nameKeys = []string{"name", "display_name", "title", "alias"}
	idKeys   = []string{"id", "model", "model_id", "slug"}
)

// parseModelsPayload 宽容解析上游 /models 响应体，返回尽力提取的能力清单。
//
// 只解析、不发请求：抽成纯函数是为了能拿各家真实响应样本做单测（不必起 http）。
// 解析不到任何能力字段的条目仍会返回（ID 非空即可），CapSource 留空 = 「未探测」。
func parseModelsPayload(raw []byte) ([]ModelCap, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("上游返回的不是合法 JSON：%w", err)
	}
	list := modelList(v)
	out := make([]ModelCap, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue // 数组里混进字符串等异常形状，跳过而不是让整次探测失败
		}
		c := ModelCap{}
		c.ID = strings.TrimSpace(firstString(m, idKeys))
		if c.ID == "" {
			continue
		}
		c.DisplayName = strings.TrimSpace(firstString(m, nameKeys))
		if c.DisplayName == c.ID {
			c.DisplayName = "" // 展示名与 ID 相同不重复下发
		}
		c.ContextWindow = firstTokens(m, ctxKeys)
		c.MaxTokens = firstTokens(m, outKeys)
		c.SupportsImages = anyCapability(m, imagesKeys, listKeys, "image", "vision")
		c.SupportsReasoning = anyCapability(m, rsnKeys, listKeys, "reasoning", "thinking", "think")
		if tools, ok := anyCapabilityOK(m, toolsKeys, listKeys, "tools", "tool_calls", "function_call", "function_calling"); ok {
			t := tools
			c.SupportsTools = &t
		}
		c.ReasoningEfforts = provider.CleanEfforts(effortList(m, effortKeys))
		c.DefaultReasoningEffort = strings.ToLower(strings.TrimSpace(firstString(m, defEff)))
		if len(c.ReasoningEfforts) == 0 && c.SupportsReasoning {
			// 声明了支持思考但没给档位表 —— 支持位仍为真，档位保持「上游未声明」（R49：不编档位列表）
			c.ReasoningEfforts = nil
		}
		// 默认档位不在上游声明的档位表里 = 上游自相矛盾，宁可不写也不能拿它去覆盖一个有效档位
		if c.DefaultReasoningEffort != "" && !containsFold(c.ReasoningEfforts, c.DefaultReasoningEffort) {
			c.DefaultReasoningEffort = ""
		}
		c.finalize()
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("上游未返回可用的模型列表（响应里没有带 id 的条目）")
	}
	return out, nil
}

// modelList 从响应体里取出模型数组：`{"data":[...]}` / `{"models":[...]}` / `[...]` 三种外层形态都吃。
func modelList(v any) []any {
	switch t := v.(type) {
	case []any:
		return t
	case map[string]any:
		for _, k := range []string{"data", "models", "items", "result"} {
			switch sub := t[k].(type) {
			case []any:
				return sub
			case map[string]any: // 再套一层（如 {"result":{"data":[...]}}）
				if l := modelList(sub); len(l) > 0 {
					return l
				}
			}
		}
	}
	return nil
}

// dig 按点分路径取嵌套字段（"architecture.input_context"）。
func dig(m map[string]any, path string) (any, bool) {
	parts := strings.Split(path, ".")
	var cur any = m
	for _, p := range parts {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = mm[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func firstString(m map[string]any, keys []string) string {
	for _, k := range keys {
		if v, ok := dig(m, k); ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	return ""
}

// firstTokens 按别名顺序取第一个能解析成 token 数的字段（数字或 "1M"/"64k" 带单位串）。
func firstTokens(m map[string]any, keys []string) int64 {
	for _, k := range keys {
		v, ok := dig(m, k)
		if !ok || v == nil {
			continue
		}
		if n, err := ParseTokens(v); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// capabilityListText 把 listKeys 里那些数组/字符串字段拼成一段小写文本，供子串匹配。
//
// 注意 `capabilities` 既可能是对象（{"image_input":true}）也可能是字符串数组，
// 这里只取字符串元素与 true 的布尔键名——布尔 false 不参与匹配，
// 否则 "supports_images":false 会被子串命中而误判成「支持识图」。
func capabilityBlob(m map[string]any, listKeys []string) string {
	var sb strings.Builder
	for _, k := range listKeys {
		v, ok := dig(m, k)
		if !ok {
			continue
		}
		switch t := v.(type) {
		case string:
			sb.WriteString(strings.ToLower(t))
			sb.WriteByte(' ')
		case []any:
			for _, e := range t {
				if s, ok := e.(string); ok {
					sb.WriteString(strings.ToLower(s))
					sb.WriteByte(' ')
				}
			}
		case map[string]any:
			for name, val := range t {
				if b, ok := val.(bool); ok && b {
					sb.WriteString(strings.ToLower(name))
					sb.WriteByte(' ')
				}
			}
		}
	}
	return sb.String()
}

// boolAt 取显式布尔声明（只有值为 true 才算「声明支持」；false/非布尔 = 未声明）。
func boolAt(m map[string]any, keys []string) (bool, bool) {
	for _, k := range keys {
		v, ok := dig(m, k)
		if !ok {
			continue
		}
		switch b := v.(type) {
		case bool:
			return b, true
		case string:
			if parsed, err := strconv.ParseBool(strings.TrimSpace(b)); err == nil {
				return parsed, true
			}
		}
	}
	return false, false
}

// anyCapability 「是否为真」：显式布尔为真，或列表字段里出现任一标记子串。
func anyCapability(m map[string]any, boolKeys, listKeys []string, markers ...string) bool {
	v, ok, _ := anyCapabilityHit(m, boolKeys, listKeys, markers...)
	return ok && v
}

// anyCapabilityOK 返回 (值, 是否声明)。上游显式写了 false 也算「声明」——
// 与「上游根本没提」不同：前者可以如实写进配置（不支持工具），后者必须留空。
func anyCapabilityOK(m map[string]any, boolKeys, listKeys []string, markers ...string) (bool, bool) {
	v, ok, hit := anyCapabilityHit(m, boolKeys, listKeys, markers...)
	if hit {
		return true, true
	}
	return v, ok
}

func anyCapabilityHit(m map[string]any, boolKeys, listKeys []string, markers ...string) (val bool, declared bool, listHit bool) {
	if b, ok := boolAt(m, boolKeys); ok {
		return b, true, false
	}
	blob := capabilityBlob(m, listKeys)
	for _, mk := range markers {
		if strings.Contains(blob, strings.ToLower(mk)) {
			return true, true, true
		}
	}
	return false, false, false
}

// effortList 取档位表：既接受 ["low","high"]，也接受 [{"name":"Low"},{"level":"high"}]。
func effortList(m map[string]any, keys []string) []string {
	for _, k := range keys {
		v, ok := dig(m, k)
		if !ok {
			continue
		}
		arr, ok := v.([]any)
		if !ok {
			continue
		}
		var out []string
		for _, e := range arr {
			switch t := e.(type) {
			case string:
				// 与下面对象分支同口径统一小写：全仓档位取值为 lowercase（gemini TierLow /
				// R63 出站注入都是小写），上游写 "Low" 也得归一，否则前端勾上后原样发出会被拒。
				out = append(out, strings.ToLower(strings.TrimSpace(t)))
			case map[string]any:
				// 档位对象：优先 type/level/name/value/id 里第一个字符串
				for _, kk := range []string{"type", "level", "value", "name", "id"} {
					if s, ok := t[kk].(string); ok && s != "" {
						// "Low"/"MEDIUM" 统一小写，与 /v1/models 的 reasoning_effort 取值口径一致
						out = append(out, strings.ToLower(s))
						break
					}
				}
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// TestConnection 拉一次上游 /models：返回尽力解析出的能力清单，兼做连通性测试。
//
// 这是**用户自己的第三方端点**，不参与内建渠道的风控账号池，因此不受 §0.1
// 「严禁穷举探测上游接口」约束；但仍然只发一个请求、不穷举多个候选端点
// （OpenRouter 式扩展字段本就在同一条 /models 响应里，没有第二个请求的必要）。
// 需要同时嗅探候选根时用 Discover（请求数仍然封在 6 次以内，见那里的注释）。
func (c *Client) TestConnection() ([]ModelCap, error) {
	caps, _, err := probeModelsAt(strings.TrimRight(c.cfg.BaseURL, "/"), c.cfg.APIKey)
	return caps, err
}

// getAt 发一个带 Bearer 的 GET。返回 (body, status, err)，status==0 只在传输层失败（DNS/连接/超时）时为 0。
//
// 上限 8 MiB（R122）：早先限 1 MiB，而 OpenRouter 这类大目录上游的 /models 响应实测 3~4 MiB，
// 截断后 json.Unmarshal 必报「不是合法 JSON」⇒「点导入模型不是每次都能列出列表」的真凶。
// 读满即判定超限并如实报错（不得静默按截断内容去解析）。
// 超时 45s（R122）：探测走的是用户自己配的接入点，经 sgproxy 时首字节常态数十秒，
// 15s 会把慢而正常的上游打成失败。
const (
	probeBodyCap = 8 << 20
	probeTimeout = 45 * time.Second
)

func getAt(u, key string) ([]byte, int, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	hc := &http.Client{Timeout: probeTimeout}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("连接失败：%v", err)
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, probeBodyCap+1))
	if rerr != nil {
		return raw, resp.StatusCode, fmt.Errorf("读取响应失败：%v", rerr)
	}
	if len(raw) > probeBodyCap {
		return raw[:probeBodyCap], resp.StatusCode, fmt.Errorf("上游响应超过 %d MiB，已放弃解析", probeBodyCap>>20)
	}
	return raw, resp.StatusCode, nil
}

// probeModelsAt GET {base}/models。只有 200 且解出至少一个带 id 的条目才算成功；
// 「200 但解不出条目」也归失败（网关把未知路径兜到首页时就是这形状）。
func probeModelsAt(base, key string) ([]ModelCap, int, error) {
	raw, status, err := getAt(base+"/models", key)
	if err != nil {
		return nil, status, err
	}
	if status != http.StatusOK {
		return nil, status, fmt.Errorf("上游返回 HTTP %d: %s", status, snippet(raw))
	}
	caps, err := parseModelsPayload(raw)
	if err != nil {
		return nil, status, err
	}
	return caps, status, nil
}

// probeChatAt GET {base}/chat/completions，只问「这条路径存在吗」，**绝不发 POST**
// （POST 到对话端点会产生真实调用与费用，嗅探阶段无权花用户的钱）。
// GET 打在 POST-only 路由上，网关一般回 405/400/415，路径不存在才回 404。
func probeChatAt(base, key string) (int, []byte, error) {
	body, status, err := getAt(base+"/chat/completions", key)
	return status, body, err
}

// chatConfirmed 判「GET /chat/completions 的这个响应能不能说明路由存在」。纯函数，便于单测。
// 2xx 只在响应体不是一张 HTML 页面时才算数：控制台类站点常对任意未知路径回 200 + SPA
// 兜底页，拿它当「端点存在」会把填错的地址说成配好了（R121 单测抓出来的真缺陷）。
func chatConfirmed(status int, body []byte) bool {
	switch {
	case status == http.StatusNotFound:
		return false
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return false // 网关在路由前就鉴权失败 ⇒ 401 既可能来自错地址也可能来自对地址，**不能当成「已确认」**（否则填错的 key 会被说成配好了）
	case status >= 200 && status < 300:
		t := strings.TrimLeft(string(body), " \t\r\n")
		return !strings.HasPrefix(t, "<")
	default:
		return true // 405/501/415/400/422/429/5xx = 路由在，只是不接受 GET 或当时异常
	}
}

// snippet 取响应体前 160 字符进错误消息（只为让人看出是 HTML 还是 JSON 错误）。
func snippet(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if r := []rune(s); len(r) > 160 {
		s = string(r[:160]) + "…"
	}
	return s
}

// Try 一次探测的如实记录。失败时逐条列进面板：用户看得懂「到底试了哪几个地址、各回什么」，
// 而不是只拿到一句「连接失败」。
type Try struct {
	URL    string `json:"url"`
	Status int    `json:"status,omitempty"`
	Err    string `json:"err,omitempty"`
}

// Discovery 嗅探结果。Base 一定是 http(s) 合法根（已规范化），调用方应把它回写进配置。
type Discovery struct {
	Base     string     `json:"base_url"`
	Renormed bool       `json:"renormed"` // 做过补协议/去尾巴之类的静态规范化
	Adjusted bool       `json:"adjusted"` // 确认的根 ≠ 用户填写规范化后的根 ⇒ 面板要提示「已自动定位」
	ModelsOK bool       `json:"models_ok"`
	ChatOK   bool       `json:"chat_ok"`
	Caps     []ModelCap `json:"-"` // 由调用方自己再序列化（handler 回 caps 字段）
	Tries    []Try      `json:"tries"`
	Note     string     `json:"note,omitempty"`
}

// Discover 规范化接入点，并在**封闭的**候选根集合里确认「哪个根 + /chat/completions 是真的」。
//
// 为什么需要它：用户粘的地址有三类歧义——漏协议头、带了 /chat/completions 尾巴、
// 「/v1 到底归不归根」。前两类可以纯静态规范化（NormalizeBaseURL），第三类只有问了上游
// 才知道，所以这里做有界嗅探：候选根最多 3 个（原样 / 加 /v1 / 去 /v1），
// 总请求数封顶 3+3=6。不猜任意路径变体（/api/v1、/openai 之类一律不试）。
//
// 判读口径（可信度从高到低）：
//  1. GET {根}/models 回 200 且解出带 id 的条目 ⇒ 命中，顺带拿到能力清单；
//  2. 所有候选的 /models 都拿不到目录时退一步：{根}/chat/completions 回**非 404 且不是 HTML 页面**
//     就认定该根存在（网关不开放目录、或 key 无目录权限，都是合法配置）。这条判据是**推断**
//     （未逐家实测），所以只在 ① 全线落空时才用，并在 Note 里写明确认依据弱；
//  3. 401/403 单独归「无从判断」：网关在路由前就鉴权失败时，401 既可能来自错地址也可能来自对地址，
//     拿它当「端点存在」会把填错的 key 说成配好了 ⇒ 直接报错让用户查 key。
func Discover(cfg ChannelConfig) (*Discovery, error) {
	norm := NormalizeBaseURL(cfg.BaseURL)
	d := &Discovery{Base: norm}
	if norm == "" {
		return d, fmt.Errorf("接入点不能为空")
	}
	if !strings.HasPrefix(norm, "http://") && !strings.HasPrefix(norm, "https://") {
		return d, fmt.Errorf("接入点 %q 不是合法的 http(s) 地址", cfg.BaseURL)
	}
	d.Renormed = !strings.EqualFold(strings.TrimSpace(cfg.BaseURL), norm)
	cands := BaseCandidates(norm)

	blocked := false // 见过 401/403 ⇒ 结论要说「密钥被拒」而不是「端点不存在」
	authBlocked := func(status int) bool {
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			blocked = true
			return true
		}
		return false
	}
	hit := func(status int) bool { return !authBlocked(status) } // ① 里用：401 不算命中目录
	rec := func(u string, status int, err error) {
		t := Try{URL: u, Status: status}
		if err != nil {
			t.Err = err.Error()
		}
		d.Tries = append(d.Tries, t)
	}

	// ① 用模型目录定位根
	for _, b := range cands {
		caps, status, err := probeModelsAt(b, cfg.APIKey)
		rec(b+"/models", status, err)
		if !hit(status) {
			continue
		}
		if err == nil && len(caps) > 0 {
			d.Base, d.ModelsOK, d.Caps = b, true, caps
			break
		}
	}
	// ② 目录拿不到 ⇒ 只确认对话端点存在。能不能算「存在」全部交给 chatConfirmed
	//（401/403 也在那里判掉），这里只额外记一份「见过鉴权被拒」用于结论措辞。
	if !d.ModelsOK {
		for _, b := range cands {
			status, body, err := probeChatAt(b, cfg.APIKey)
			rec(b+"/chat/completions", status, err)
			authBlocked(status)
			if err != nil || !chatConfirmed(status, body) {
				continue
			}
			d.Base, d.ChatOK = b, true
			d.Note = "上游未开放模型目录，仅按 /chat/completions 端点存在确认了接入点（模型与能力需手填）"
			break
		}
	}
	if !d.ModelsOK && !d.ChatOK {
		if blocked {
			return d, fmt.Errorf("候选地址均回 401/403：api-key 被上游拒绝，端点是否正确无从判断（请核对密钥）")
		}
		return d, fmt.Errorf("没能定位到可用的 OpenAI 兼容端点：试了 %d 个地址，均无 /chat/completions", len(d.Tries))
	}
	// ③ 目录命中时也补一次对话端点确认（目录与对话分家挂在不同前缀下的网关并不罕见）
	if d.ModelsOK {
		status, body, err := probeChatAt(d.Base, cfg.APIKey)
		rec(d.Base+"/chat/completions", status, err)
		d.ChatOK = err == nil && chatConfirmed(status, body)
		switch {
		case err != nil:
			d.Note = fmt.Sprintf("模型目录可用，但确认对话端点失败：%v", err)
		case d.ChatOK:
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			d.Note = fmt.Sprintf("模型目录可用，但对话端点回 HTTP %d：网关可能对其单独鉴权，未能确认（不影响已定位的接入点）", status)
		default:
			d.Note = fmt.Sprintf("模型目录在 %s 可用，但该根下 /chat/completions 未确认存在（HTTP %d），对话可能挂在别的前缀下", d.Base, status)
		}
	}
	d.Adjusted = !strings.EqualFold(d.Base, norm)
	return d, nil
}

// containsFold 不区分大小写的成员判定（档位已统一小写，这里只防上游脏数据）。
func containsFold(list []string, v string) bool {
	for _, s := range list {
		if strings.EqualFold(s, v) {
			return true
		}
	}
	return false
}
