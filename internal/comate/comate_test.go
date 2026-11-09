package comate

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// ---------------------------------------------------------------------------
// 真实 wire 形态（2026-10-27，经真实 wps_sid 直测 comate.wps.cn）
// ---------------------------------------------------------------------------

// mockFrame 是 comate 特有的「业务错误伪装成正常回答」形态：HTTP 200 + 一帧
// 看起来完全正常的 SSE，但 id 前缀 chatcmpl-mock-、model=="mock"。
// 判据必须卡死这两个字段（见 sse.go peekStream）。
const mockFrame = `data: {"id":"chatcmpl-mock-1793156969187","object":"chat.completion.chunk",` +
	`"model":"mock","choices":[{"index":0,"delta":{"content":"指定模型暂不可用，请切换其他模型后重试"},` +
	`"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}` +
	"\n\ndata: [DONE]\n\n"

// notLoginBody 是 401 的真实形态（bogus cookie 实测）。
const notLoginBody = `{"error":{"message":"未登录，请先登录","type":"authentication_error","code":"not_login"}}`

// quotaBody 是无效模型的非流式真实形态：**HTTP 200** + 业务错误信封（code 字段）。
const quotaBody = `{"code":"MULTIMODAL_TOKEN_QUOTA_EXCEEDED","data":"指定模型暂不可用，请切换其他模型后重试",` +
	`"msg":"指定模型暂不可用，请切换其他模型后重试"}`

// ---------------------------------------------------------------------------
// Classify：会话失效 → 积分耗尽 → 模型不可用 → 状态码
// ---------------------------------------------------------------------------

func TestClassify(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   provider.ErrKind
	}{
		{"401 not_login", 401, notLoginBody, provider.ErrSessionDead},
		{"401 登录校验失败", 401, `{"error":{"message":"登录校验失败","type":"authentication_error","code":"not_login"}}`, provider.ErrSessionDead},
		{"403", 403, `{"error":{"message":"forbidden"}}`, provider.ErrSessionDead},
		{"429", 429, `{"error":{"message":"too many requests"}}`, provider.ErrSoftRate},
		{"402 积分", 402, `{"error":{"message":"payment required"}}`, provider.ErrHardCredit},
		{"200+14018 积分耗尽", 200, `{"error":{"message":"Credits exhausted","code":14018}}`, provider.ErrHardCredit},
		{"200+积分不足中文", 200, `{"error":{"message":"积分不足，请前往 WPS 会员中心充值"}}`, provider.ErrHardCredit},
		{"200+指定模型暂不可用", 200, quotaBody, provider.ErrNotFound},
		{"404", 404, `{"error":{"message":"not found"}}`, provider.ErrNotFound},
		{"500", 500, `{"error":{"message":"internal"}}`, provider.ErrServer},
		{"502", 502, `bad gateway`, provider.ErrServer},
		{"未知形态透传", 400, `{"error":{"message":"bad request"}}`, provider.ErrClient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.status, tc.body)
			if got != tc.want {
				t.Fatalf("Classify(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// mock 帧检测：peekStream 必须把 200+mock 帧转成 ErrNotFound 的 StreamError
// ---------------------------------------------------------------------------

func TestPeekStreamDetectsMockFrame(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(mockFrame))
	}))
	defer srv.Close()

	c := New()
	c.Base = srv.URL
	rc, status, _, err := c.ChatStream(fakeAuth(), chatBody("673940726/zhipu/glm-5.3-flash//public"))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer rc.Close()
	if status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}
	// 读流必须得到 StreamError（kind=ErrNotFound），而不是把 mock 文本当回答吐出去
	buf := make([]byte, 4096)
	n, rerr := rc.Read(buf)
	if rerr == nil {
		t.Fatalf("expected StreamError from mock frame, got content %q", buf[:n])
	}
	se, ok := rerr.(*StreamError)
	if !ok {
		t.Fatalf("read err = %T (%v), want *comate.StreamError", rerr, rerr)
	}
	if se.Kind() != provider.ErrNotFound {
		t.Fatalf("StreamError kind = %v, want ErrNotFound", se.Kind())
	}
	if !strings.Contains(se.Error(), "指定模型暂不可用") {
		t.Fatalf("StreamError msg = %q, want 含「指定模型暂不可用」", se.Error())
	}
}

// 正常流不得被 peekStream 误杀：首帧照常透传。
func TestPeekStreamPassesNormalStream(t *testing.T) {
	const normalFrame = `data: {"id":"chatcmpl-real-1","model":"glm-5.3-flash",` +
		`"choices":[{"index":0,"delta":{"content":"你好"},"finish_reason":null}]}` + "\n\n" +
		`data: [DONE]` + "\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(normalFrame))
	}))
	defer srv.Close()

	c := New()
	c.Base = srv.URL
	rc, status, _, err := c.ChatStream(fakeAuth(), chatBody("673940726/zhipu/glm-5.3-flash//public"))
	if err != nil || status != 200 {
		t.Fatalf("ChatStream: err=%v status=%d", err, status)
	}
	defer rc.Close()
	buf := make([]byte, 4096)
	n, rerr := rc.Read(buf)
	if rerr != nil {
		t.Fatalf("normal stream read: %v", rerr)
	}
	if !strings.Contains(string(buf[:n]), "chatcmpl-real-1") {
		t.Fatalf("first chunk lost: %q", buf[:n])
	}
}

// ---------------------------------------------------------------------------
// 鉴权形态：Cookie-only（Bearer 无效）—— applyHeaders 只设 Cookie
// ---------------------------------------------------------------------------

func TestApplyHeadersCookieOnly(t *testing.T) {
	var gotCookie, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"code":0,"msg":"","data":{"items":[]}}`))
	}))
	defer srv.Close()

	c := New()
	c.Base = srv.URL
	_, _ = c.getJSON(fakeAuth(), EpModels)
	if gotCookie != CookieName+"="+fakeSID {
		t.Fatalf("Cookie = %q, want %q", gotCookie, CookieName+"="+fakeSID)
	}
	if gotAuth != "" {
		t.Fatalf("Authorization must be empty (comate rejects Bearer), got %q", gotAuth)
	}
}

// ---------------------------------------------------------------------------
// FetchModels：items 原样透传 + 能力字段填充（ContextFromAPI=true）
// ---------------------------------------------------------------------------

func TestFetchModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"","data":{"auto":{},"items":[` +
			`{"display_name":"GLM-5.3 Flash","name":"673940726/zhipu/glm-5.3-flash//public",` +
			`"llm_types":["llm-chat"],"context_window":1000000,"support_thinking":true,` +
			`"thinking_levels":["low","high"],"model_tier":"flash","consume_ratio":0.04},` +
			`{"display_name":"DeepSeek V4.1 Flash","name":"673940726/deepseek/deepseek-v4.1-flash//public",` +
			`"llm_types":["llm-multimodal"],"context_window":1000000,"support_thinking":true,` +
			`"thinking_levels":null,"model_tier":"flash","consume_ratio":0.05}` +
			`]}}`))
	}))
	defer srv.Close()

	c := New()
	c.Base = srv.URL
	mis, err := c.FetchModels(fakeAuth())
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(mis) != 2 {
		t.Fatalf("models = %d, want 2", len(mis))
	}
	// 排序是字典序：deepseek < zhipu
	glm := mis[1]
	if glm.ID != "673940726/zhipu/glm-5.3-flash//public" {
		t.Fatalf("ID = %q（不得改名）", glm.ID)
	}
	if glm.Name != "GLM-5.3 Flash" {
		t.Fatalf("Name = %q", glm.Name)
	}
	if glm.ContextWindow != 1000000 || !glm.ContextFromAPI {
		t.Fatalf("ContextWindow = %d ContextFromAPI = %v", glm.ContextWindow, glm.ContextFromAPI)
	}
	if !glm.SupportsReasoning {
		t.Fatal("SupportsReasoning 应为 true")
	}
	if len(glm.ReasoningEfforts) != 2 || glm.ReasoningEfforts[0] != "low" {
		t.Fatalf("ReasoningEfforts = %v", glm.ReasoningEfforts)
	}
	ds := mis[0]
	if ds.ID != "673940726/deepseek/deepseek-v4.1-flash//public" {
		t.Fatalf("ds ID = %q", ds.ID)
	}
	if !ds.SupportsImages {
		t.Fatal("llm-multimodal 应置 SupportsImages")
	}
	// knownIDs 已记住：目录拉取失败时兜底返回缓存
	c.Base = srv.URL + "/broken"
	cached, err := c.FetchModels(fakeAuth())
	if err != nil || len(cached) != 2 {
		t.Fatalf("缓存兜底失败: err=%v n=%d", err, len(cached))
	}
}

// ---------------------------------------------------------------------------
// ValidateSID：零 token 验证 + 401 人话错误
// ---------------------------------------------------------------------------

func TestValidateSID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != CookieName+"="+fakeSID {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(notLoginBody))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"msg":"","data":{"items":[]}}`))
	}))
	defer srv.Close()

	c := New()
	c.Base = srv.URL
	uid, models, err := c.validateSID(fakeSID)
	if err != nil {
		t.Fatalf("ValidateSID: %v", err)
	}
	if uid == "" || !strings.HasPrefix(uid, "sid-") {
		t.Fatalf("uid = %q, want sid-<sha256[:16]>", uid)
	}
	if models != 0 {
		t.Fatalf("models = %d, want 0（items 为空）", models)
	}
	// 同 sid 稳定
	uid2, _, _ := c.validateSID(fakeSID)
	if uid2 != uid {
		t.Fatalf("uid 不稳定: %q vs %q", uid, uid2)
	}
	// bogus sid → 人话错误（不是裸 HTTP 401）
	if _, _, err := c.validateSID("bogus"); err == nil || !strings.Contains(err.Error(), "无效") {
		t.Fatalf("bogus sid err = %v, want 含「无效」的人话错误", err)
	}
}

// TestValidateSIDCountsCatalog 目录条数必须如实上报：R135 用它判断
// 「这个账号到底有没有 Comate 模型」，为 0 时不能放进路由池。
func TestValidateSIDCountsCatalog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"","data":{"items":[` +
			`{"name":"673940726/zhipu/glm-5.3-flash//public"},` +
			`{"name":"673940726/deepseek/deepseek-v4-pro//public"}]}}`))
	}))
	defer srv.Close()

	c := New()
	c.Base = srv.URL
	uid, models, err := c.validateSID(fakeSID)
	if err != nil {
		t.Fatalf("validateSID: %v", err)
	}
	if uid == "" || models != 2 {
		t.Fatalf("uid=%q models=%d, want uid non-empty + models=2", uid, models)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

const fakeSID = "V02FakeSidForUnitTests0000000000000000000000000001"

func fakeAuth() *auth.Auth {
	return &auth.Auth{Kind: "comate", UID: "sid-test", AccessToken: fakeSID}
}

func chatBody(model string) []byte {
	b, _ := json.Marshal(map[string]any{
		"model":  model,
		"stream": true,
		"messages": []map[string]any{
			{"role": "user", "content": "hi"},
		},
	})
	return b
}

// ---------------------------------------------------------------------------
// 2026-10-28 修复回归：peekStream 必须增量判定（不得攒批）+ prettifyID 单段兜底
// ---------------------------------------------------------------------------

// 首帧到达即交付：旧实现用 io.ReadAll(LimitReader(body, 64KB))，要等读满 64KB 或 EOF，
// 实测首帧 flush 后上游停 1.5s，客户端首次 Read 也被推迟 1.5s（所有对话退化成
// 「正文突然整段出现」）。此处断言：上游写出首帧后阻塞 1s，客户端必须立刻拿到。
func TestPeekStreamDeliversFirstFrameWithoutWaiting(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"id":"chatcmpl-real-1","choices":[{"delta":{"content":"早"}}]}` + "\n\n"))
		w.(http.Flusher).Flush()
		<-release // 首帧之后一直不发（模拟慢思考）
	}))
	defer srv.Close()
	defer close(release)

	c := New()
	c.Base = srv.URL
	rc, status, _, err := c.ChatStream(fakeAuth(), chatBody("673940726/zhipu/glm-5.3-flash//public"))
	if err != nil || status != 200 {
		t.Fatalf("ChatStream: err=%v status=%d", err, status)
	}
	defer rc.Close()

	start := time.Now()
	buf := make([]byte, 4096)
	n, rerr := rc.Read(buf)
	elapsed := time.Since(start)
	if rerr != nil {
		t.Fatalf("read: %v", rerr)
	}
	if !strings.Contains(string(buf[:n]), "chatcmpl-real-1") {
		t.Fatalf("首帧丢失: %q", buf[:n])
	}
	if elapsed > 400*time.Millisecond {
		t.Fatalf("首帧交付延迟 %v（应≈0，旧实现会等到 1s 后的 release）", elapsed)
	}
}

// 首字节前的空闲必须有兜底：旧实现 IdleReader 包在窥视之后，上游 100 后不发字节会永久挂住。
func TestPeekStreamIdleGuardCoversBeforeFirstByte(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-release // 一个字节都不发
	}))
	defer srv.Close()
	defer close(release)

	c := New()
	c.Base = srv.URL
	c.IdleTimeout = 100 * time.Millisecond
	rc, _, _, err := c.ChatStream(fakeAuth(), chatBody("673940726/zhipu/glm-5.3-flash//public"))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer rc.Close()

	start := time.Now()
	buf := make([]byte, 64)
	if _, rerr := rc.Read(buf); rerr == nil {
		t.Fatal("上游不发字节应报空闲超时，实际读成功")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("空闲兜底失效：等了 %v", elapsed)
	}
}

// 单段 ID（无 "/"）不得 panic：旧实现 parts[len(parts)-2] 越界。
// 触发路径：上游 display_name 为空且 name 无斜杠 ⇒ 整个 FetchModels 崩。
func TestPrettifyIDSingleSegment(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"glm-5.3-flash", "Glm 5.3 flash"},
		{"673940726/zhipu/glm-5.3-flash//public", "Glm 5.3 flash"},
		{"", ""},
		{"//", ""},
	} {
		if got := prettifyID(tc.in); got != tc.want {
			t.Fatalf("prettifyID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// display_name 为空 + 单段 name ⇒ FetchModels 不得崩（旧实现此处 panic）。
func TestFetchModelsEmptyDisplayNameSingleSegment(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"items":[{"display_name":"","name":"glm-5.3-flash"}]}}`))
	}))
	defer srv.Close()
	c := New()
	c.Base = srv.URL
	mis, err := c.FetchModels(fakeAuth())
	if err != nil || len(mis) != 1 {
		t.Fatalf("FetchModels: err=%v n=%d", err, len(mis))
	}
	if mis[0].ID != "glm-5.3-flash" {
		t.Fatalf("ID = %q（不得改名）", mis[0].ID)
	}
	if mis[0].Name == "" {
		t.Fatal("display_name 为空时应由 prettifyID 兜底出展示名")
	}
}

// ---------------------------------------------------------------------------
// 粘贴容错（R131）：用户常把整段 Cookie / DevTools 表格一行 / 带引号的值一起粘进来
// ---------------------------------------------------------------------------

func TestNormalizeSID(t *testing.T) {
	const sid = "V02FakeSidForUnitTests0000000000000000000000000001"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"裸值", sid, sid},
		{"前后空白", "  " + sid + "\n", sid},
		{"双引号", `"` + sid + `"`, sid},
		{"单引号", `'` + sid + "'", sid},
		{"k=v 单条", "wps_sid=" + sid, sid},
		{"k=v 带结尾分号", "wps_sid=" + sid + ";", sid},
		{"整段 Cookie 头", "Cookie: OTHER=1; wps_sid=" + sid + "; ANOTHER=2", sid},
		{"小写 cookie 头", "cookie: wps_sid=" + sid + "; x=1", sid},
		{"DevTools 表格行（Tab 分隔）", "wps_sid\t" + sid + "\t.wps.cn\t/\t2027-01-01\t50", sid},
		{"DevTools 列名 + 空格", "wps_sid " + sid, sid},
		{"空串", "   ", ""},
		{"只有前缀", "wps_sid=", ""},
	}
	for _, c := range cases {
		if got := NormalizeSID(c.in); got != c.want {
			t.Errorf("%s: NormalizeSID(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

// ValidateSID 必须容忍粘贴噪声：上游收到的 Cookie 只能是规整后的裸值。
func TestValidateSIDAcceptsPastedCookie(t *testing.T) {
	var gotCookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		_, _ = w.Write([]byte(`{"code":0,"msg":"","data":{"items":[]}}`))
	}))
	defer srv.Close()

	c := New()
	c.Base = srv.URL
	if _, _, err := c.validateSID(NormalizeSID("Cookie: a=1; wps_sid=" + fakeSID + "; b=2")); err != nil {
		t.Fatalf("validateSID: %v", err)
	}
	if want := CookieName + "=" + fakeSID; gotCookie != want {
		t.Fatalf("Cookie = %q, want %q（噪声必须被剥掉）", gotCookie, want)
	}
}

// TestFetchModelsEmptyDirKeepsCache 锁死「空目录不得清空面板」。
//
// 实测事故：面板里加了第二个 WPS 账号（该账号 Comate 未开通 ⇒ /models 恒返回
// items=[]），handler 的模型缓存是按单个账号 Pick 出来的，一次 Pick 命中空账号，
// 整个 comate 渠道的 9 个模型瞬间归零、要等 5 分钟负缓存才恢复。
// 修法：空目录一律回落到已知缓存；缓存也空时返回错误（日志里能看出是账号没开通）。
func TestFetchModelsEmptyDirKeepsCache(t *testing.T) {
	full := `{"code":0,"data":{"items":[{"display_name":"GLM-5.3","name":"673940726/zhipu/glm-5.3//public"}]}}`
	empty := `{"code":0,"data":{"items":[],"total":0}}`
	serve := full
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(serve))
	}))
	defer srv.Close()

	c := New()
	c.Base = srv.URL
	a := fakeAuth()
	if mis, err := c.FetchModels(a); err != nil || len(mis) != 1 {
		t.Fatalf("首次拉取: err=%v n=%d", err, len(mis))
	}

	serve = empty // 上游改口：这个账号没有模型
	mis, err := c.FetchModels(a)
	if err != nil {
		t.Fatalf("空目录不得报错（应回落到缓存）: %v", err)
	}
	if len(mis) != 1 || mis[0].ID != "673940726/zhipu/glm-5.3//public" {
		t.Fatalf("空目录应回落到缓存，实际 n=%d %+v", len(mis), mis)
	}

	// 缓存也空 ⇒ 必须报错（否则面板静默变空、日志无线索）。
	c2 := New()
	c2.Base = srv.URL
	if _, err := c2.FetchModels(a); err == nil {
		t.Fatal("冷启动遇到空目录应返回错误")
	}
}

// ---------------------------------------------------------------------------
// 积分/额度（R136）：真实 wire 形态见 credits.go 注释
// ---------------------------------------------------------------------------

// tokenUsageEnterprise 企业号实测形态（每日额度 1000 / 已用 142.63 / 剩余 1857.37）。
const tokenUsageEnterprise = `{"code":0,"msg":"","data":{"companyId":"673940726","userId":"1568907578",` +
	`"credits":{"allocations":[],"daily_limit":{"limit":1000,"remain":1857.37,"used":142.63},` +
	`"mixed_priority":"credits","seat":null,"usage_mode":"mixed"},` +
	`"credits_usage":{"periods":[{"credits_limit":1000,"credits_rest":1857.37,"credits_used":142.63,` +
	`"period_label":"每日","period_type":"daily"}]}}}`

// tokenUsagePersonal 个人号（未开通 Comate）实测形态：daily_limit 为 null，
// periods 两项 limit/rest 全为 -1 —— 「没有额度」而不是「用光」。
const tokenUsagePersonal = `{"code":0,"msg":"","data":{"companyId":"0","userId":"224720862",` +
	`"credits":{"allocations":[],"daily_limit":null,"mixed_priority":"credits","usage_mode":"mixed"},` +
	`"credits_usage":{"periods":[{"credits_limit":-1,"credits_rest":-1,"credits_used":0,` +
	`"period_label":"每日","period_type":"daily"},{"credits_limit":-1,"credits_rest":-1,"credits_used":0,` +
	`"period_label":"每月","period_type":"monthly"}]}}}`

// creditsFake 起一个假上游：/token-usage 与 /users/current 走 Origin，
// /models 与 users/current 分别在 llmproxy 与主域根；两个 origin 都指向同一台 httptest。
// trialBody 是 PSY 企业号（642757510）的真实试用额度形态：
// JSON 数值是缩放后的原始值（scale=3），precision 里才是人读的 "100.00"/"5.24"。
const trialBody = `{"code":0,"msg":"success","data":{"trial_available":true,"with_precision":true,` +
	`"trial_info":{"period_type":"","period_value":0,"value":100000,"expire_time":1795772189,` +
	`"consumed":9470,"precision":{"value":"100.00","consumed":"5.24","scale":3}}}}`

// trialNone 是老账号（673940726）的形态：没有试用包。
const trialNone = `{"code":0,"msg":"success","data":{"trial_available":false}}`

func creditsFake(t *testing.T, tokenUsage string, trial ...string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 三个端点都必须带 Cookie 鉴权（Bearer 一律 401，见包注释实测事实 1）。
		if r.Header.Get("Cookie") != CookieName+"="+fakeSID {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(notLoginBody))
			return
		}
		if r.Header.Get("Authorization") != "" {
			t.Errorf("%s 不应带 Authorization（comate 只认 Cookie）", r.URL.Path)
		}
		switch r.URL.Path {
		case "/llmproxy/v1/user" + EpTokenUsage: // 幂等：必须挂在 APIBase 前缀下
			_, _ = w.Write([]byte(tokenUsage))
		case EpTrialCredits:
			body := trialNone
			if len(trial) > 0 {
				body = trial[0]
			}
			_, _ = w.Write([]byte(body))
		case EpUserCurrent:
			_, _ = w.Write([]byte(`{"code":0,"data":{"user_id":1568907578,"nickname":"李柠希",` +
				`"company_name":"西南大学","current_company_id":673940726,"status":"active"}}`))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	c := New()
	// Base 带真实前缀：token-usage 必须拼在 /llmproxy/v1/user 之后
	// （拼到主域根会拿到一页 HTML，R136 实测踩过）。
	c.Base = srv.URL + "/llmproxy/v1/user"
	c.Origin = srv.URL // users/current 在主域根
	return c
}

// TestUserResourceEnterpriseQuota 主积分 = 每日额度剩余（1857.37 → 1857）。
func TestUserResourceEnterpriseQuota(t *testing.T) {
	c := creditsFake(t, tokenUsageEnterprise)
	remain, err := c.UserResource(&auth.Auth{AccessToken: fakeSID})
	if err != nil {
		t.Fatalf("UserResource: %v", err)
	}
	if remain != 1857 {
		t.Fatalf("remain = %d, want 1857（每日额度剩余 1857.37 四舍五入）", remain)
	}
}

// TestUserResourcePersonalNoQuota 个人号：上游回 -1 / null，必须回 (0, nil)
// —— 「没有额度」是中性事实，回 error 会让面板显示「刷新失败」。
func TestUserResourcePersonalNoQuota(t *testing.T) {
	c := creditsFake(t, tokenUsagePersonal)
	remain, err := c.UserResource(&auth.Auth{AccessToken: fakeSID})
	if err != nil {
		t.Fatalf("个人号（无额度）不应报错: %v", err)
	}
	if remain != 0 {
		t.Fatalf("remain = %d, want 0（周期额度 -1 表示没有额度）", remain)
	}
}

// TestUserResourceDetailDailyOnly 明细只有每日额度这一条：
// 「智点」是灵犀渠道的口径，comate 绝不能掺进来（用户明确纠正过）。
func TestUserResourceDetailDailyOnly(t *testing.T) {
	c := creditsFake(t, tokenUsageEnterprise)
	remain, items, err := c.UserResourceDetail(&auth.Auth{AccessToken: fakeSID})
	if err != nil {
		t.Fatalf("UserResourceDetail: %v", err)
	}
	if remain != 1857 {
		t.Fatalf("主积分 = %d, want 1857", remain)
	}
	if len(items) != 1 {
		t.Fatalf("明细 %d 条, want 1（只有每日额度）: %+v", len(items), items)
	}
	daily := items[0]
	if daily.Key != "daily" || daily.Total != 1000 || daily.Used != 143 || daily.Remain != 1857 {
		t.Fatalf("每日额度条目不对: %+v", daily)
	}
	if daily.ExpireAt == "" {
		t.Fatal("每日额度应带当日有效期（面板据此算「即将过期」）")
	}
	for _, it := range items {
		if it.Key != "daily" {
			t.Fatalf("不得出现非额度条目（智点属灵犀渠道）: %+v", it)
		}
	}
}

// TestQuotaItemsSkipsNoLimit 上游的 -1 不是 0：既不能算成「0 可用」，也不能塞条目。
func TestQuotaItemsSkipsNoLimit(t *testing.T) {
	d := &tokenUsageData{}
	d.CreditsUsage.Periods = []creditPeriod{
		{Limit: -1, Rest: -1, Label: "每日", Type: "daily"},
		{Limit: -1, Rest: -1, Label: "每月", Type: "monthly"},
	}
	items, primary := quotaItems(d)
	if len(items) != 0 || primary != 0 {
		t.Fatalf("items=%d primary=%d, want 0/0", len(items), primary)
	}
}

// TestUserResourceTokenUsageError 上游 5xx 必须是 error（与「没有额度」区分开）。
func TestUserResourceTokenUsageError(t *testing.T) {
	c := creditsFake(t, "")
	if _, err := c.UserResource(&auth.Auth{AccessToken: fakeSID}); err == nil {
		t.Fatal("上游 404 应返回 error")
	}
}

// TestFetchProfileDisplayName 昵称补全：企业号显示「昵称（企业名）」。
func TestFetchProfileDisplayName(t *testing.T) {
	c := creditsFake(t, tokenUsageEnterprise)
	nick, company, err := c.fetchProfile(&auth.Auth{AccessToken: fakeSID})
	if err != nil {
		t.Fatalf("fetchProfile: %v", err)
	}
	if got := DisplayName(nick, company); got != "李柠希（西南大学）" {
		t.Fatalf("DisplayName = %q", got)
	}
	if got := DisplayName("dear life", ""); got != "dear life" {
		t.Fatalf("个人号 DisplayName = %q", got)
	}
	if got := DisplayName("", ""); got != "" {
		t.Fatalf("空身份应回空串（调用方回退 uid 尾号），got %q", got)
	}
}

// TestTrialAmountsUsesPrecision 数值必须按 precision 还原。
//
// 上游 JSON 里 value=100000 是 scale=3 的原始值，直接当积分数会显示 100000；
// 只看 value 不看 precision 是这类「带精度字段」接口最常见的踩坑。
func TestTrialAmountsUsesPrecision(t *testing.T) {
	info := &trialInfo{Value: 100000, Consumed: 9470}
	info.Precision.Value = "100.00"
	info.Precision.Consumed = "5.24"
	info.Precision.Scale = 3
	total, used, remain := trialAmounts(info)
	if total != 100 || used != 5.24 || math.Abs(remain-94.76) > 1e-9 {
		t.Fatalf("trialAmounts = (%v,%v,%v), want (100, 5.24, 94.76)", total, used, remain)
	}
	// 没有 precision 时按 scale 还原。
	bare := &trialInfo{Value: 100000, Consumed: 9470}
	 bare.Precision.Scale = 3
	if total, used, _ := trialAmounts(bare); total != 100 || math.Abs(used-9.47) > 1e-9 {
		t.Fatalf("无 precision 时 scale 还原失败: (%v,%v)", total, used)
	}
}

// TestUserResourceFallsBackToTrial 每日额度为 -1 的账号，主积分必须回退到免费额度。
//
// PSY 企业号就是这一种：token-usage 的 daily_limit=null、periods 全 -1，
// 但 billing/v3/credits/trial 有 100-5.24；只看 token-usage 会误判成「没有额度」。
func TestUserResourceFallsBackToTrial(t *testing.T) {
	c := creditsFake(t, tokenUsagePersonal, trialBody)
	remain, err := c.UserResource(&auth.Auth{AccessToken: fakeSID})
	if err != nil {
		t.Fatalf("UserResource: %v", err)
	}
	if remain != 95 {
		t.Fatalf("remain = %d, want 95（94.76 四舍五入）", remain)
	}
}

// TestUserResourceDetailIncludesTrialAsSeparateItem 免费额度独立成条，且不顶掉每日额度。
func TestUserResourceDetailIncludesTrialAsSeparateItem(t *testing.T) {
	// 每日额度在时：主积分仍是每日，免费额度只作明细。
	c := creditsFake(t, tokenUsageEnterprise, trialBody)
	primary, items, err := c.UserResourceDetail(&auth.Auth{AccessToken: fakeSID})
	if err != nil {
		t.Fatalf("UserResourceDetail: %v", err)
	}
	if primary != 1857 {
		t.Fatalf("primary = %d, want 1857（每日额度优先）", primary)
	}
	var trial *provider.ResourceItem
	for i := range items {
		if items[i].Key == "trial" {
			trial = &items[i]
		}
	}
	if trial == nil {
		t.Fatalf("明细里缺 trial 条目: %+v", items)
	}
	if trial.Total != 100 || trial.Used != 5 || trial.Remain != 95 {
		t.Fatalf("trial 条目 = %+v, want total 100 used 5 remain 95", *trial)
	}
	if !strings.HasPrefix(trial.ExpireAt, "2026-") || !strings.Contains(trial.ExpireAt, "T") {
		t.Fatalf("trial 有效期应为 RFC3339 精确时刻，实际 %q", trial.ExpireAt)
	}
	if trial.Name != "免费额度" {
		t.Fatalf("名称应为「免费额度」（客户端同文案），实际 %q", trial.Name)
	}
}

// TestUserResourceTrialAbsentKeepsZero 没有试用包的账号：主积分 0，不报错。
func TestUserResourceTrialAbsentKeepsZero(t *testing.T) {
	c := creditsFake(t, tokenUsagePersonal, trialNone)
	remain, err := c.UserResource(&auth.Auth{AccessToken: fakeSID})
	if err != nil {
		t.Fatalf("UserResource: %v", err)
	}
	if remain != 0 {
		t.Fatalf("remain = %d, want 0", remain)
	}
}

// TestUnknownModelRejectedAsClientError 本地目录校验必须回 400（ErrClient），不能回 404。
//
// 实测事故（2026-10-29 11:04）：用另一个租户的模型 id 请求时，本地 knownModel 判定失败回
// 404 → Classify 归成 ErrNotFound（可换号）→ handler 换到下一个账号，本地照样拒 → 每次轮换
// 都记一笔账号错误，两个号各记两笔后整条渠道进入冷却，正常请求也被拒。这个错误是纯本地判定
// （上游一次都没碰），属于调用方参数错误：既不换号、也不该罚账号健康度。
func TestUnknownModelRejectedAsClientError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"","data":{"items":[{"name":"673940726/zhipu/glm-5.3-flash//public","display_name":"GLM-5.3 Flash"}]}}`))
	}))
	defer srv.Close()

	c := New()
	c.Base = srv.URL
	if _, err := c.FetchModels(fakeAuth()); err != nil {
		t.Fatalf("FetchModels: %v", err)
	}

	rc, status, body, err := c.ChatStream(fakeAuth(), chatBody("642757510/deepseek/deepseek-v4.1-flash//public"))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if rc != nil {
		_ = rc.Close()
		t.Fatalf("未命中目录的模型不应返回流")
	}
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400（404 会被当成账号级 not_found 触发换号与冷却）", status)
	}
	if kind := Classify(status, string(body)); kind != provider.ErrClient {
		t.Fatalf("Classify = %v, want ErrClient（不得换号、不得记账号错误）", kind)
	}
}
