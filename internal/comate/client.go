package comate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// Client 实现 provider.Upstream。
//
// 与 cline 同为「粘贴型」渠道：凭据是 wps_sid Cookie（面板粘贴，见 app.SubmitComateSID），
// Cookie 不轮换 ⇒ 无 RefreshToken、无保活、无签到。
type Client struct {
	HTTP *http.Client
	// StreamHTTP 专供对话流：**不设 Client.Timeout**（R35）。
	// Client.Timeout 覆盖整个请求生命周期（含读 body），SSE 长回答必被从中间掐断。
	// 空闲兜底由 provider.NewIdleReader 承担（见 openStream）。
	StreamHTTP *http.Client
	// IdleTimeout 流式空闲上限，0 = DefaultIdleTimeout。
	IdleTimeout time.Duration
	// Base 网关基址（默认 APIBase）；测试注入 httptest 假上游用，生产不设。
	Base string
	// Origin 主域（默认 OriginComate）：/token-usage、/api/coserve/v1/users/current
	// 不在 llmproxy 前缀下，走这个域。测试注入 httptest 用。
	Origin string

	knownMu  sync.RWMutex
	knownIDs map[string]string // 上游完整 ID → 展示名（目录缓存 + ChatStream 本地校验集）
}

// New 构造客户端（非流式 120s 总超时；一问一答需要兜底，故保留 Timeout）。
func New() *Client {
	tr := newTransport()
	return &Client{
		HTTP:       &http.Client{Timeout: 120 * time.Second, Transport: tr},
		StreamHTTP: &http.Client{Transport: tr},
		IdleTimeout: DefaultIdleTimeout,
		Base:       defaultBaseURL,
		knownIDs:   map[string]string{},
	}
}

// newTransport 出厂 Transport：与 cline 同款（禁 h2 + ResponseHeaderTimeout 兜底）。
// StreamHTTP 无总超时后**必须**有 ResponseHeaderTimeout，否则连响应头都等不到会无限挂。
func newTransport() *http.Transport {
	return &http.Transport{
		DisableKeepAlives:     false,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 90 * time.Second,
		DisableCompression:    false,
	}
}

func (c *Client) base() string {
	if c.Base != "" {
		return strings.TrimRight(c.Base, "/")
	}
	return defaultBaseURL
}

// originOf 主域（默认 OriginComate）。
func (c *Client) originOf() string {
	if c.Origin != "" {
		return strings.TrimRight(c.Origin, "/")
	}
	return OriginComate
}

func (c *Client) idleTimeout() time.Duration {
	if c.IdleTimeout > 0 {
		return c.IdleTimeout
	}
	return DefaultIdleTimeout
}

// applyHeaders 注入凭据。⚠️ 本渠道**只认 Cookie**（见包注释实测事实 1）：
// Authorization Bearer 实测 401（apiKey、sid 均无效）；且 Cookie 在场时任意
// bogus Authorization 头不影响 200 —— 所以只设 Cookie，不设 Authorization。
func applyHeaders(req *http.Request, sid string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if sid != "" {
		req.Header.Set("Cookie", CookieName+"="+sid)
	}
}

// sidOf 从 Auth 取凭据（wps_sid 存于 AccessToken）。
func sidOf(a *auth.Auth) string {
	if a == nil {
		return ""
	}
	return strings.TrimSpace(a.AccessTokenValue())
}

// ---------------------------------------------------------------------------
// 对话
// ---------------------------------------------------------------------------

// ChatStream 实现 provider.Upstream：返回上游流（200）或已读尽的错误响应。
//
// body 是客户端原始请求（model 已是上游完整 ID，由 handler 剥掉 comate/ 前缀后传入）。
// 返回的流经 openStream 包装（IdleReader 空闲兜底，R35）。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	m := modelOf(body)
	if m == "" {
		return nil, http.StatusBadRequest, []byte(`{"error":{"message":"model is required"}}`), nil
	}
	// 本地校验：集合 == 暴露集 == /models items（构造即一致）；目录未拉到过时放行
	//（fail-open，R42：校验过严会误拒上游新模型）。
	//
	// 状态码必须是 **400 而不是 404**：404 会被 Classify 归成 ErrNotFound，而这个错误是
	// **本地**判定的请求错误（模型 id 不在我们已经拉到的目录里），上游一次都没碰。用 404
	// 会让 handler 认为「这个号不行」去换号，换到另一个号照样被本地拒，每次轮换都记一笔账号
	// 错误 ⇒ 实测两个号各记两笔后整条渠道进入冷却，正常请求也被拒（11:04 事故）。
	// 400 走 badParams ⇒ ErrClient：不换号、不罚账号，直接把「模型 id 不对」告诉调用方。
	if !c.knownModel(m) {
		return nil, http.StatusBadRequest,
			[]byte(fmt.Sprintf(`{"error":{"message":"model not found in comate catalog: %s","code":"model_not_found"}}`, jsonEscape(m))), nil
	}

	out := clampMaxTokens(body)
	out = forceStream(out)
	sid := sidOf(a)

	req, err := http.NewRequest(http.MethodPost, c.base()+EpChat, bytes.NewReader(out))
	if err != nil {
		return nil, http.StatusInternalServerError, nil, err
	}
	applyHeaders(req, sid)

	resp, err := c.StreamHTTP.Do(req)
	if err != nil {
		return nil, http.StatusInternalServerError, nil, err
	}

	if resp.StatusCode != http.StatusOK {
		// 非 200：读尽 body 交调用方分类（本渠道业务错误常压平成 200，但真 HTTP 错误仍在）。
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, resp.StatusCode, raw, nil
	}

	// 200 但 body 可能是「mock 帧」错误（见包注释实测事实 2）。
	// peekStream 窥视首段字节：命中 mock 判据则转成可分类的 StreamError，
	// 否则把已读字节与剩余流拼接后放行。
	return newPeekStream(resp.Body, m, c.idleTimeout()), http.StatusOK, nil, nil
}

// openStream 把 200 响应包成带空闲兜底的流（R35：不用 Client.Timeout，靠 IdleReader）。
// peekStream 内部已含 IdleReader 包装（见 newPeekStream）。
func (c *Client) openStream(resp *http.Response) io.ReadCloser {
	return provider.NewIdleReader(resp.Body, c.idleTimeout())
}

// modelOf 从请求 body 提取 model 字段（不整包 Unmarshal，避免大 body 的无谓解析）。
func modelOf(body []byte) string {
	var v struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &v) != nil {
		return ""
	}
	return strings.TrimSpace(v.Model)
}

// clampMaxTokens 把客户端的 max_tokens 夹到上游窗口 [1, 32768]。
// 客户端不传时**不添加**该字段（实测不传最稳；上游 default 16384 自理会）。
func clampMaxTokens(body []byte) []byte {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return body
	}
	v, ok := obj["max_tokens"]
	if !ok {
		return body
	}
	f, ok := toFloat(v)
	if !ok {
		return body
	}
	mt := int64(f)
	if mt < minUpstreamMaxTokens {
		mt = minUpstreamMaxTokens
	} else if mt > maxUpstreamMaxTokens {
		mt = maxUpstreamMaxTokens
	}
	obj["max_tokens"] = mt
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// forceStream 强制上游 stream:true（不变量 1，与 cline 同款）。
//
// **为什么必须有**：handler 对上游响应**一律按 SSE 解析**——流式走 Stream、
// 非流式客户端走 Aggregate，二者都只认 `data:` 帧。客户端发 stream:false 时
// 上游会回非流式 JSON 信封，解析器找不到任何 data 帧 ⇒ 502 upstream_parse，
// 且该错误会被当作账号连续错误 ⇒ 冷却整条渠道（cline 的实测教训）。
// 非流式客户端由 handler 本地聚合（Aggregate），与 cline/oczen 同款做法。
func forceStream(body []byte) []byte {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return body
	}
	if v, _ := obj["stream"].(bool); v {
		return body // 已是 true，零改动原样返回（不重新 marshal，避免无谓的键序变化）
	}
	obj["stream"] = true
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

// jsonEscape 转义字符串用于内嵌 JSON 错误文案。
func jsonEscape(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// getJSON 发 GET 并返回原始 body（信封校验在调用方）。
func (c *Client) getJSON(a *auth.Auth, path string) (json.RawMessage, error) {
	req, err := http.NewRequest(http.MethodGet, c.base()+path, nil)
	if err != nil {
		return nil, err
	}
	applyHeaders(req, sidOf(a))
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("comate: %s HTTP %d: %s", path, resp.StatusCode, truncate(string(raw), 512))
	}
	return raw, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------------------------------------------------------------------------
// ValidateSID：面板添加账号入口（零 token 消耗）
// ---------------------------------------------------------------------------

// NormalizeSID 把用户粘贴的内容规整成裸 wps_sid 值。
//
// 面板要求的是「整条 Cookie 里 wps_sid 的值」，但普通用户的常见操作是：
//
//   - 在 DevTools 的 Cookie 表格里连列名一起复制 → `wps_sid  V02S...`（Tab 分隔）
//   - 直接复制整行 `wps_sid=V02S...;`
//   - 复制整个 Cookie 请求头 → `Cookie: OTHER=1; wps_sid=V02S...; ANOTHER=2`
//   - 多复制了引号、换行、结尾分号
//
// 以上都包含正确值，只是夹了噪声。不规整就直接丢给上游的话，用户看到的是
// 「wps_sid 无效或已过期」——像是自己没登录，实际只是复制多了几个字符，
// 这种失败对普通用户来说完全无从自查，故在这里一次性剥干净。
func NormalizeSID(raw string) string {
	s := strings.TrimSpace(raw)
	// 整段 Cookie 头：`Cookie:` 前缀（大小写不敏感）先摘掉。
	if len(s) > 7 && strings.EqualFold(s[:7], "cookie:") {
		s = strings.TrimSpace(s[7:])
	}
	// 定位 wps_sid= 或 wps_sid<TAB>/<空格>（DevTools 表格复制是 Tab 分隔）。
	lower := strings.ToLower(s)
	if i := strings.Index(lower, "wps_sid="); i >= 0 {
		s = s[i+len("wps_sid="):]
	} else if i := strings.Index(lower, "wps_sid"); i >= 0 {
		// 列名形态：跳过 "wps_sid" 与其后的分隔符（Tab/空格/冒号）。
		s = strings.TrimLeft(s[i+len("wps_sid"):], " \t:：")
	}
	// 取到下一个 Cookie 分隔符为止。
	if j := strings.IndexAny(s, ";,\t\r\n "); j >= 0 {
		s = s[:j]
	}
	// 引号、结尾分号是复制时最常见的尾巴。
	s = strings.TrimSpace(s)
	s = strings.Trim(s, `"'`)
	return s
}

// ValidateSID 校验 wps_sid 并取回身份（面板添加账号入口，见 app.SubmitComateSID）。
//
// 只调 GET /models —— **零 token 消耗、零副作用**，避免「添加账号即烧积分」。
// sid 无效时上游回 401 not_login（实测），据此给出人话错误。
// uid 用 sid 的 sha256 前 16 位：上游 /models 不回传任何身份字段（实测），
// 但 uid 只需稳定且不含明文 —— 与 cline probeKey 的 hash 兜底同款。
func ValidateSID(raw string) (uid string, err error) {
	sid := NormalizeSID(raw)
	cli := New()
	uid, _, err = cli.validateSID(sid)
	return uid, err
}

// ValidateSIDWithModels 校验凭据并返回该账号当前的模型数量。
//
// `/models` 是**账号维度**目录：`code:0` 且 `items:[]` 表示这个 WPS 账号在 Comate 侧
// 一个模型都没有（实测个人号：「注册了 Comate」但没有分配任何模型），并非凭据失效。
// 调用方据此决定要不要把账号放进路由池。
func ValidateSIDWithModels(raw string) (uid string, models int, err error) {
	sid := NormalizeSID(raw)
	cli := New()
	return cli.validateSID(sid)
}

// validateSID 是 ValidateSID 的可注入版本（测试用 httptest 假上游替换 Base）。
func (c *Client) validateSID(sid string) (uid string, models int, err error) {
	sid = strings.TrimSpace(sid)
	if sid == "" {
		return "", 0, fmt.Errorf("wps_sid 不能为空")
	}
	req, err := http.NewRequest(http.MethodGet, c.base()+EpModels, nil)
	if err != nil {
		return "", 0, err
	}
	applyHeaders(req, sid)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 128<<10))
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		if resp.StatusCode != http.StatusOK {
			return "", 0, fmt.Errorf("校验 wps_sid 失败 HTTP %d：%s", resp.StatusCode, truncate(strings.TrimSpace(string(raw)), 256))
		}
		// 200：再验信封 code==0（上游业务错误也可能压平成 200，见包注释实测事实 2）。
		var env struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
			Data struct {
				Items []json.RawMessage `json:"items"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &env) == nil && env.Code != 0 {
			return "", 0, fmt.Errorf("wps_sid 无效：%s", env.Msg)
		}
		sum := sha256Sum(sid)
		return "sid-" + sum[:16], len(env.Data.Items), nil
	}
	return "", 0, fmt.Errorf("wps_sid 无效或已过期（HTTP %d）：请重新从浏览器复制", resp.StatusCode)
}

// ---------------------------------------------------------------------------
// Upstream 其余方法
// ---------------------------------------------------------------------------

// RefreshToken 空实现：wps_sid Cookie **不轮换**。
// 因此不变量 20（refresh 后必须 SaveAtomic）在本渠道不适用，
// 且 R26/R44 那种「同凭据多处使用互踢」的问题天然不存在。
// ExpiresAt 由 SubmitComateSID 置 FarFutureExpiry，NeedsRefresh 恒假。
func (c *Client) RefreshToken(a *auth.Auth) error { return nil }

// UserResource / UserResourceDetail 见 credits.go（R136）：
// 额度真实端点是 /llmproxy/v1/user/token-usage（每日额度），
// 早期「/credits /userinfo /me 全 404 ⇒ 恒 0」的结论已作废。

// DailyCheckin 无签到活动。调度器侧 CheckinHours 传 []int{}（R39），
// 且 noExplicitCheckin 会挡掉面板手工签到，故本方法正常情况下不会被调用。
func (c *Client) DailyCheckin(a *auth.Auth) error { return fmt.Errorf("comate 无签到活动") }

// FetchModelPricing 见 models.go（R136）：倍率来自 /models 的 consume_ratio。

// Classify 实现 provider.Upstream。
func (c *Client) Classify(status int, body string) provider.ErrKind { return Classify(status, body) }

var _ provider.Upstream = (*Client)(nil)
