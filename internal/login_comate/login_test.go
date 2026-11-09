// login_test.go 锁定扫码登录/浏览器接管的**纯逻辑**部分（不触网）。
//
// 网络路径（qr.wps.cn 三个接口、CDP 接管）在真实环境验证过，单测只覆盖那些
// 「写错了不会报错、只会静默出错」的地方：JSONP 包装的剥离、Cookie 里挑 sid、
// 会话状态机对外的可见性（面板轮询就读这些字段）。
package logincomate

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wild-work/internal/cdp"
)

// TestJSONPBodyStripsWrapper 上游一律回 JSONP（`cb({...})`），
// 少剥一层会让 json.Unmarshal 失败、面板永远停在「正在申请二维码」。
func TestJSONPBodyStripsWrapper(t *testing.T) {
	cases := []struct{ in, want string }{
		{`cb({"state":"pending"})`, `{"state":"pending"}`},
		{`cb ({"state":"pending"})`, `{"state":"pending"}`},
		{`  cb({"state":"pending"});  `, `{"state":"pending"}`},
		{`{"state":"pending"}`, `{"state":"pending"}`}, // 上游偶尔回裸 JSON
		{``, ``},
	}
	for _, c := range cases {
		if got := jsonpBody(c.in); got != c.want {
			t.Errorf("jsonpBody(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestSIDOfPicksWpsSid Cookie 列表里可能有十几个 Cookie，只能取 wps_sid；
// 取错一个（如 kso_sid）就等于把别的会话凭据写进账号文件。
func TestSIDOfPicksWpsSid(t *testing.T) {
	cookies := []cdp.Cookie{
		{Name: "kso_sid", Value: "wrong"},
		{Name: "wps_sid", Value: "right"},
		{Name: "wps_sid_extra", Value: "also-wrong"},
	}
	if got := sidOf(cookies); got != "right" {
		t.Errorf("应取 wps_sid，得到 %q", got)
	}
	if got := sidOf(nil); got != "" {
		t.Errorf("无 Cookie 时应为空，得到 %q", got)
	}
	// 空值不得当作凭据（否则会拿空 sid 去校验并在面板报「sid 无效」）
	if got := sidOf([]cdp.Cookie{{Name: "wps_sid", Value: ""}}); got != "" {
		t.Errorf("空值 wps_sid 不应作为凭据，得到 %q", got)
	}
}

// TestQRSessionSnapshotStates 面板靠 Snapshot 的 state 文案驱动 UI，
// 状态机要保证：初始 pending、失败带 error、取消后不再回报 pending。
func TestQRSessionSnapshotStates(t *testing.T) {
	s := &QRSession{cancel: func() {}}
	s.set(QRStatePending, nil, "用手机 WPS 扫码")
	snap := s.Snapshot()
	if snap["state"] != QRStatePending {
		t.Fatalf("初始状态应为 %q，得到 %v", QRStatePending, snap["state"])
	}
	if _, ok := snap["error"]; ok {
		t.Error("pending 时不应带 error 字段（前端会当成失败）")
	}

	s.set(QRStateFailed, errors.New("二维码申请失败"), "")
	snap = s.Snapshot()
	if snap["state"] != QRStateFailed {
		t.Fatalf("应为 failed，得到 %v", snap["state"])
	}
	if snap["error"] != "二维码申请失败" {
		t.Errorf("error 文案应透传，得到 %v", snap["error"])
	}

	s.set(QRStateCancelled, nil, "")
	if snap := s.Snapshot(); snap["state"] != QRStateCancelled {
		t.Errorf("取消后应为 cancelled，得到 %v", snap["state"])
	}
}

// TestQRSessionSuccessCarriesAccount 成功后必须回传 uid/nickname，
// 面板据此提示「已添加哪个账号」；丢了这些字段用户不知道加的是谁。
func TestQRSessionSuccessCarriesAccount(t *testing.T) {
	s := &QRSession{cancel: func() {}}
	s.succeed(Result{UID: "sid-abc123", Nickname: "WPS 账号 abc123", Warn: "该 sid 已存在"})
	snap := s.Snapshot()
	if snap["state"] != QRStateSuccess {
		t.Fatalf("应为 success，得到 %v", snap["state"])
	}
	if snap["uid"] != "sid-abc123" || snap["nickname"] != "WPS 账号 abc123" {
		t.Errorf("成功快照缺账号信息: %+v", snap)
	}
	if snap["warning"] != "该 sid 已存在" {
		t.Errorf("非致命告警应透传，得到 %v", snap["warning"])
	}
}

// ---------------------------------------------------------------------------
// CSRF / PKCE（R137）：扫码确认后换不出 wps_sid 的真凶
// ---------------------------------------------------------------------------

// TestNewCSRFToken 字符表与长度必须与登录页一致（上游可能做格式校验）。
func TestNewCSRFToken(t *testing.T) {
	tok := newCSRFToken()
	if len(tok) != 32 {
		t.Fatalf("csrf 长度 = %d, want 32", len(tok))
	}
	for _, r := range tok {
		if !strings.ContainsRune(csrfAlphabet, r) {
			t.Fatalf("csrf 含字符表之外的字符 %q: %s", r, tok)
		}
	}
	if newCSRFToken() == tok {
		t.Fatal("两次生成的 csrf 相同（随机源有问题）")
	}
}

// TestPKCEChallengeS256 RFC 7636 附录 B 的标准向量：
// verifier "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
// → challenge "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"。
func TestPKCEChallengeS256(t *testing.T) {
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	const want = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	if got := challengeOf(verifier); got != want {
		t.Fatalf("challenge = %q, want %q（必须是 S256 + 无填充 base64url）", got, want)
	}
	v, c := newPKCEPair()
	if len(v) < 43 || len(v) > 128 {
		t.Fatalf("verifier 长度 %d 不在 RFC 7636 允许的 43~128", len(v))
	}
	if c != challengeOf(v) {
		t.Fatal("newPKCEPair 的 challenge 与 verifier 不匹配")
	}
}

// fakeAccount 起一个假账号域：要求 X-CSRFToken 与 csrf Cookie 同时存在且一致，
// 否则按真实上游回 403 csrfRequire；命中则下发 wps_sid Cookie。
func fakeAccount(t *testing.T, sid string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/session/exchange/login" {
			w.WriteHeader(404)
			return
		}
		header := r.Header.Get(csrfHeader)
		var cookie string
		if c, err := r.Cookie(csrfCookieName); err == nil {
			cookie = c.Value
		}
		if header == "" || cookie == "" || header != cookie {
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"result":"csrfRequire","msg":"非法访问（错误码：0x00018）"}`))
			return
		}
		http.SetCookie(w, &http.Cookie{Name: CookieName, Value: sid, Path: "/"})
		_, _ = w.Write([]byte(`{"result":"ok"}`))
	}))
	t.Cleanup(srv.Close)
	old := accountBase
	accountBase = srv.URL
	t.Cleanup(func() { accountBase = old })
	return srv
}

// TestExchangeSessionSendsCSRF 回归护栏：缺 CSRF 时上游 403 csrfRequire，
// 扫码确认后一条凭据都换不回来（R137 前的真实故障）。
func TestExchangeSessionSendsCSRF(t *testing.T) {
	const sid = "fake-wps-sid-for-test"
	fakeAccount(t, sid)
	q, err := newQRHTTP()
	if err != nil {
		t.Fatalf("newQRHTTP: %v", err)
	}
	got, err := exchangeSession(context.Background(), q, qrLoginResp{AuthCode: "one-time-code"}, "verifier-xyz")
	if err != nil {
		t.Fatalf("exchangeSession: %v（真因是 CSRF 头没带）", err)
	}
	if got != sid {
		t.Fatalf("sid = %q, want %q", got, sid)
	}
}

// TestExchangeSessionReportsUpstreamBody 失败时错误里必须带上游响应体：
// 只写「未下发 wps_sid」会让排障从 30 秒变成两次扫码。
func TestExchangeSessionReportsUpstreamBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"result":"csrfRequire","msg":"非法访问（错误码：0x00018）"}`))
	}))
	defer srv.Close()
	old := accountBase
	accountBase = srv.URL
	defer func() { accountBase = old }()

	q, err := newQRHTTP()
	if err != nil {
		t.Fatalf("newQRHTTP: %v", err)
	}
	_, err = exchangeSession(context.Background(), q, qrLoginResp{AuthCode: "code"}, "v")
	if err == nil {
		t.Fatal("上游 403 应返回错误")
	}
	if !strings.Contains(err.Error(), "csrfRequire") {
		t.Fatalf("错误信息里应含上游原文 csrfRequire，实际: %v", err)
	}
}

// TestResetPendingAfterRefresh 换新二维码必须把会话复位成「等待扫码」。
//
// 实测事故：用户扫码后 86ms 二维码被自动换新，会话既不前进也不报错，
// 面板一直写着「已扫码，请确认」，而用户确认的是已经作废的那张码。
func TestResetPendingAfterRefresh(t *testing.T) {
	s := &QRSession{cancel: func() {}}
	s.mu.Lock()
	s.state = QRStateScanned
	s.hint = "已扫码，请在手机 WPS 上确认登录"
	s.qrURL = "https://qr.wps.cn/api/qrcode/png?data=old"
	s.mu.Unlock()

	s.resetPending("https://qr.wps.cn/api/qrcode/png?data=new")

	snap := s.Snapshot()
	if snap["state"] != QRStatePending {
		t.Fatalf("换新后 state = %v, want %q（否则面板仍显示上一张码的「已扫码」）", snap["state"], QRStatePending)
	}
	if snap["qr"] != "https://qr.wps.cn/api/qrcode/png?data=new" {
		t.Fatalf("换新后二维码没更新: %v", snap["qr"])
	}
	if h, _ := snap["hint"].(string); !strings.Contains(h, "扫码") {
		t.Fatalf("换新后提示应回到「扫码」，实际: %q", h)
	}
}
