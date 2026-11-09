package lingxi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
// 与 comate 同为「粘贴型」渠道：凭据是 wps_sid Cookie（面板粘贴，见
// app.SubmitLingxiSID），Cookie 不轮换 ⇒ 无 RefreshToken、无保活；
// 但灵犀有每日签到（领智点）与余额端点。
type Client struct {
	HTTP *http.Client
	// StreamHTTP 专供对话流：**不设 Client.Timeout**（R35）。
	// SSE 长回答必被总超时从中间掐断，空闲兜底由 provider.NewIdleReader 承担。
	StreamHTTP *http.Client
	// IdleTimeout 流式空闲上限，0 = DefaultIdleTimeout。
	IdleTimeout time.Duration
	// Base 网关基址（默认 APIBase）；测试注入 httptest 假上游用，生产不设。
	Base string

	knownMu  sync.RWMutex
	knownIDs map[string]string // 上游完整 ID → 展示名（目录缓存 + ChatStream 本地校验集）
}

// New 构造客户端（非流式 120s 总超时；一问一答需要兜底，故保留 Timeout）。
func New() *Client {
	tr := newTransport()
	return &Client{
		HTTP:        &http.Client{Timeout: 120 * time.Second, Transport: tr},
		StreamHTTP:  &http.Client{Transport: tr},
		IdleTimeout: DefaultIdleTimeout,
		Base:        defaultBaseURL,
		knownIDs:    map[string]string{},
	}
}

// newTransport 出厂 Transport：与 comate 同款（禁 h2 + ResponseHeaderTimeout 兜底）。
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

func (c *Client) idleTimeout() time.Duration {
	if c.IdleTimeout > 0 {
		return c.IdleTimeout
	}
	return DefaultIdleTimeout
}

// applyHeaders 注入基础凭据头（Cookie-only，Bearer 实测无效，与 comate 一致）。
func applyHeaders(req *http.Request, sid string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if sid != "" {
		req.Header.Set("Cookie", CookieName+"="+sid)
	}
}

// applyChatHeaders 在基础头上追加 completions 三专用头（实测事实 3）。
func applyChatHeaders(req *http.Request, sid string) {
	applyHeaders(req, sid)
	req.Header.Set("X-Cc-Version", ccVersion)
	req.Header.Set("X-Client-Type", ccClientTyp)
	req.Header.Set("X-Cc-Entrance", ccEntrance)
}

// sidOf 从 Auth 取凭据（wps_sid 存于 AccessToken）。
func sidOf(a *auth.Auth) string {
	if a == nil {
		return ""
	}
	return strings.TrimSpace(a.AccessTokenValue())
}

// ---------------------------------------------------------------------------
// 请求体改写：OpenAI messages → 灵犀 question
// ---------------------------------------------------------------------------

// chatRequest 灵犀 completions 请求体形态。
type chatRequest struct {
	Question        string    `json:"question"`
	Model           string    `json:"model"`
	UserInput       userInput `json:"user_input"`
	ReasoningEffort string    `json:"reasoning_effort,omitempty"`
}

type userInput struct {
	Text string `json:"text"`
}

// openAIMessage 是客户端请求（OpenAI 形）messages 数组元素的按需子集。
type openAIMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// buildChatRequest 把 OpenAI 形 body 转成灵犀请求体。
//
// messages 压缩规则（magpie 同款）：灵犀接口只收单个 question 字符串——
// 单轮取该条 content；多轮把历史拼成 `[System: …]`/`User: …`/`Assistant: …`
// 前缀段落，最后追加 `\n\nUser: <最后一条 user 消息>`。content 非字符串
// （多模态数组等）时 JSON 序列化原文交上游处置。
//
// reasoning_effort（实测事实 5）：low→"0" / medium|high→"1" / max→"2"；
// 不认识或未传 ⇒ 省略字段交上游默认。
//
// 注意（2026-10-29 审查 P2）：灵犀协议**不收 max_tokens**——客户端传值原样
// 丢弃，不透传（上游输出长度由模型默认决定）。
func buildChatRequest(body []byte) ([]byte, error) {
	var src struct {
		Model           string          `json:"model"`
		Messages        []openAIMessage `json:"messages"`
		ReasoningEffort string          `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, err
	}
	if strings.TrimSpace(src.Model) == "" {
		return nil, fmt.Errorf("model is required")
	}

	question := compressMessages(src.Messages)
	if question == "" {
		question = "你好"
	}

	out := chatRequest{
		Question:  question,
		Model:     strings.TrimSpace(src.Model),
		UserInput: userInput{Text: question},
	}
	if e := mapEffort(strings.TrimSpace(src.ReasoningEffort)); e != "" {
		out.ReasoningEffort = e
	}
	return json.Marshal(out)
}

// compressMessages 把多轮 messages 压成单条 question 字符串（magpie 同构）。
func compressMessages(msgs []openAIMessage) string {
	if len(msgs) == 0 {
		return ""
	}
	// 最后一条 user 消息（从尾部找）。
	lastUserIdx := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			lastUserIdx = i
			break
		}
	}
	currentQ := ""
	if lastUserIdx >= 0 {
		currentQ = contentText(msgs[lastUserIdx].Content)
	}
	// 单轮（或只有一条 user）：直接返回该条内容。
	if len(msgs) == 1 {
		return contentText(msgs[0].Content)
	}
	// 多轮：历史拼前缀段落。
	var history []string
	for i, m := range msgs {
		if i == lastUserIdx {
			break // 只拼最后一条 user 之前的段落；最后 user 本条与其后的消息（含尾随
			// assistant）不进历史——最后 user 的内容已在 currentQ，尾随 assistant
			// 丢弃（magpie 同款）。2026-10-29 审查 S-1：原注释误称「含其后
			// assistant 也并入」，与 break 语义相反。
		}
		c := contentText(m.Content)
		switch m.Role {
		case "system":
			history = append(history, "[System: "+c+"]")
		case "user":
			history = append(history, "User: "+c)
		case "assistant":
			history = append(history, "Assistant: "+c)
		}
	}
	if len(history) == 0 {
		return currentQ
	}
	return strings.Join(history, "\n\n") + "\n\nUser: " + currentQ
}

// contentText 取 message.content 的文本形态：字符串原样；非字符串（数组等）
// JSON 原文交上游处置（magpie 同款）。
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

// ---------------------------------------------------------------------------
// 会话创建 + 对话
// ---------------------------------------------------------------------------

// createSession 新建对话会话，返回 session_id。
// 同一 session 并发会回 error 帧（magpie 注释同款）⇒ 每请求各建一个。
// 副作用：上游侧 session 记录会随对话次数积累（协议无删除端点，magpie 亦
// 未处理）——本侧无法根治，仅在此标注（2026-10-29 审查 P3）。
func (c *Client) createSession(sid string) (string, error) {
	req, err := http.NewRequest(http.MethodPost, c.base()+EpSessions, strings.NewReader("{}"))
	if err != nil {
		return "", err
	}
	applyHeaders(req, sid)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("lingxi: 会话创建失败 HTTP %d: %s", resp.StatusCode, truncate(string(raw), 256))
	}
	var env struct {
		Result string `json:"result"`
		Data   struct {
			SessionID string `json:"session_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", fmt.Errorf("lingxi: 会话创建响应解析失败: %w", err)
	}
	if env.Result != "" && env.Result != "ok" {
		return "", fmt.Errorf("lingxi: 会话创建失败: %s", env.Result)
	}
	if strings.TrimSpace(env.Data.SessionID) == "" {
		return "", fmt.Errorf("lingxi: 会话创建未返回 session_id")
	}
	return env.Data.SessionID, nil
}

// ChatStream 实现 provider.Upstream：先建会话，再 POST completions，返回上游流。
//
// body 是客户端原始请求（model 已是上游完整 ID，由 handler 剥掉 lingxi/ 前缀后
// 传入）。上游恒为 SSE（不变量 1：本渠道 completions 只会回 SSE，无需 forceStream）；
// 流的形态转换（灵犀帧 → OpenAI 帧）在 sse.go 的 Stream/Aggregate 完成，此处
// 只负责拿到原始流并包 IdleReader。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	m := modelOf(body)
	if m == "" {
		return nil, http.StatusBadRequest, []byte(`{"error":{"message":"model is required"}}`), nil
	}
	// 本地校验（R42 fail-open：目录未拉到过时放行，误拦等于渠道周期性瘫痪）。
	//
	// 状态码必须是 **400 而不是 404**（R141，与 comate R139 同款）：404 会被 Classify
	// 归成 ErrNotFound ⇒ handler 认为「这个号不行」去换号，换到另一个号照样被本地拒，
	// 每次轮换都记一笔账号错误 ⇒ 两号各记两笔后整条渠道进冷却，正常请求也被拒。
	// 400 走 badParams ⇒ ErrClient：不换号、不罚账号，直接把「模型 id 不对」告诉调用方。
	if !c.knownModel(m) {
		return nil, http.StatusBadRequest,
			[]byte(fmt.Sprintf(`{"error":{"message":"model not found in lingxi catalog: %s","code":"model_not_found"}}`, jsonEscape(m))), nil
	}

	out, err := buildChatRequest(body)
	if err != nil {
		return nil, http.StatusBadRequest, []byte(`{"error":{"message":"` + jsonEscape(err.Error()) + `"}}`), nil
	}
	sid := sidOf(a)

	// 每请求新建会话（实测事实 2）。
	sessionID, err := c.createSession(sid)
	if err != nil {
		return nil, http.StatusBadGateway, []byte(`{"error":{"message":"` + jsonEscape(err.Error()) + `"}}`), nil
	}

	req, err := http.NewRequest(http.MethodPost, c.base()+EpCompletions(sessionID), bytes.NewReader(out))
	if err != nil {
		return nil, http.StatusInternalServerError, nil, err
	}
	applyChatHeaders(req, sid)

	resp, err := c.StreamHTTP.Do(req)
	if err != nil {
		return nil, http.StatusInternalServerError, nil, err
	}

	if resp.StatusCode != http.StatusOK {
		// 非 200：读尽 body 交调用方分类。
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, resp.StatusCode, raw, nil
	}

	// 200：业务错误也可能压在 SSE 流内的 error 帧里（event:error 或 type:error），
	// 由 sse.go 的转换器识别；此处把原始流包 IdleReader（空闲兜底，R35）。
	return provider.NewIdleReader(resp.Body, c.idleTimeout()), http.StatusOK, nil, nil
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
		return nil, fmt.Errorf("lingxi: %s HTTP %d: %s", path, resp.StatusCode, truncate(string(raw), 512))
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
// ValidateSID：面板添加账号入口（零智点消耗）
// ---------------------------------------------------------------------------

// ValidateSID 校验 wps_sid 并取回身份（面板添加账号入口，见 app.SubmitLingxiSID）。
//
// 只调 GET /api/aioffice/v1/sessions/plans —— **零智点消耗、零副作用**，避免
// 「添加账号即烧积分」。sid 无效时上游回 HTTP 401（balance 端点同款形态）。
// uid 用 sid 的 sha256 前 16 位：上游不回传任何身份字段，但 uid 只需稳定
// 且不含明文 —— 与 comate 的 hash 兜底同款。
func ValidateSID(sid string) (uid string, err error) {
	cli := New()
	return cli.validateSID(sid)
}

// validateSID 是 ValidateSID 的可注入版本（测试用 httptest 假上游替换 Base）。
func (c *Client) validateSID(sid string) (uid string, err error) {
	sid = strings.TrimSpace(sid)
	if sid == "" {
		return "", fmt.Errorf("wps_sid 不能为空")
	}
	// 兼容整段 Cookie 粘贴（wps_sid=V02… 形态）：**大小写不敏感**提取等号后的值。
	// 2026-10-29 审查 S-3 修复：原实现 ToLower 只用于匹配、TrimPrefix 仍作用于
	// 原串 ⇒ 粘贴大写 WPS_SID=… 时判定通过却剥不掉前缀；同时改为按分号切分，
	// 一并兼容 wps_sid 混在多 Cookie 串中间的形态（other=1; wps_sid=V02…; path=/）。
	for _, part := range strings.Split(sid, ";") {
		part = strings.TrimSpace(part)
		eq := strings.Index(part, "=")
		if eq == len(CookieName) && strings.EqualFold(part[:eq], CookieName) {
			sid = strings.TrimSpace(part[eq+1:])
			break
		}
	}
	req, err := http.NewRequest(http.MethodGet, c.base()+EpPlans, nil)
	if err != nil {
		return "", err
	}
	applyHeaders(req, sid)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 128<<10))
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("校验 wps_sid 失败 HTTP %d：%s", resp.StatusCode, truncate(strings.TrimSpace(string(raw)), 256))
		}
		// 200：再验信封（plans 的权威信封是 data 节点 + result/v7code）。
		var env struct {
			Result string `json:"result"`
			Data   struct {
				Models []json.RawMessage `json:"models"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			return "", fmt.Errorf("wps_sid 校验响应解析失败：%w", err)
		}
		if env.Result == "UserUnauthorized" {
			return "", fmt.Errorf("wps_sid 无效或已过期：请重新从浏览器复制")
		}
		// uid 必须带**渠道前缀**：同一 wps_sid 在 comate 渠道也会入库，若两渠道
		// 用同一 "sid-<hash>" 生成 uid，面板所有按 uid 定位账号的操作（签到/刷新积分/
		// 移除/禁用/改昵称/明细）会随机命中 map 遍历到的先一个渠道——实测表现为
		// 点「刷新」灵犀账号，日志却是 platform=comate（2026-10-29 t8 真机验收发现）。
		return "lxsid-" + sha256Sum(sid)[:16], nil
	}
	return "", fmt.Errorf("wps_sid 无效或已过期（HTTP %d）：请重新从浏览器复制", resp.StatusCode)
}

// sha256Sum 返回 hex 摘要（uid 生成用）。
func sha256Sum(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// Upstream 其余方法
// ---------------------------------------------------------------------------

// RefreshToken 空实现：wps_sid Cookie **不轮换**。
// ExpiresAt 由 SubmitLingxiSID 置 FarFutureExpiry，NeedsRefresh 恒假。
func (c *Client) RefreshToken(a *auth.Auth) error { return nil }

// FetchModelPricing 上游无费率端点；单价只在 multiplier 文案里（～0.12x），
// 由 models.go 记进 ModelInfo.Name，不进费率表。返回空不报错。
func (c *Client) FetchModelPricing(a *auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}

// Classify 实现 provider.Upstream。
func (c *Client) Classify(status int, body string) provider.ErrKind { return Classify(status, body) }

// 确认实现接口（编译期断言，与 comate 同款）。
var _ provider.Upstream = (*Client)(nil)
