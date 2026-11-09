// qr.go WPS Comate「扫码登录」——全程服务端 HTTP，不需要桌面、不需要浏览器。
//
// 与 auto.go 的 CDP 路径相比，这条路解决的是**部署环境问题**：CDP 只能拉起
// 「运行 wild-work 的那台机器」的浏览器。NAS / docker / systemd 里没有桌面，
// 窗口永远弹不出来；而用户自己浏览器里的 wps_sid 又因为同源策略读不到。
// 扫码登录把「拿凭据」这件事整个搬到服务端：
//
//	① 服务端向 qr.wps.cn 申请一个登录二维码（loginid）
//	② 面板把二维码图片显示出来，用户用手机 WPS Office 扫
//	③ 服务端长轮询扫码状态，手机确认后拿到一次性 authcode
//	④ authcode 换 session Cookie，捞到 wps_sid → 校验 → 落盘
//
// 这条流程与 WPS 网页版登录页（account.wps.cn/login）用的是同一套接口，
// 区别只是网页版由浏览器 JS 完成 ④，这里由服务端完成。
//
// 拼图是实测出来的：`GET /api/v3/login_qrcode` 出 loginid，
// `GET /api/v3/login_qrcode/url` 出二维码图，`GET /api/v3/login_qrcode/login`
// 长轮询出 state（pending → scan → logined）/authcode（见 docs/决议项.md R132）。
package logincomate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"
)

// CookieName Comate 凭据所在的 Cookie 名。
const CookieName = "wps_sid"

// Result 登录成功后的账号信息。
type Result struct {
	// UID 由 sid 的哈希推出（上游 /models 不回传身份字段）。
	UID string
	// Nickname 展示用昵称。
	Nickname string
	// Warn 同账号等非致命告警（空串表示无）。
	Warn string
}

const (
	// qrAPIBase 二维码服务（与 account.wps.cn 分离的独立域）。
	qrAPIBase = "https://qr.wps.cn"
	// accountBase 默认的 WPS 账号域：换 session Cookie 在这里。
	// 单独拎成变量（不是常量）只为单元测试能把它指向 httptest 假上游。
	accountBaseDefault = "https://account.wps.cn"
	// qrTimeout 整条扫码流程的最长等待（二维码过期会自动换新）。
	qrTimeout = 5 * time.Minute
	// qrPollTimeout 单次长轮询的 HTTP 超时（上游会挂起到用户扫码，给足余量）。
	qrPollTimeout = 45 * time.Second
	// qrRefreshGap 二维码主动换新间隔（上游 TTL 约 2 分钟，留出余量）。
	qrRefreshGap = 110 * time.Second

	// csrfCookieName / csrfHeader 是 WPS 账号域的双提交 CSRF 形态（见 newQRHTTP）。
	//
	// 实测：不带它 POST /api/session/exchange/login 直接 403
	// `{"result":"csrfRequire","msg":"非法访问（错误码：0x00018）"}`——
	// 这就是「扫码确认了却换不出 wps_sid」的真凶：三个换取分支里第一个就被 CSRF 挡掉，
	// 而当时把响应体扔了，日志只说「上游未下发 wps_sid」。
	csrfCookieName = "csrf"
	csrfHeader     = "X-CSRFToken"
	// csrfAlphabet 与登录页 common.createCsrftoken 的字符表逐字符一致（32 位）。
	csrfAlphabet = "ABCDEFGHJKMNPQRSTWXYZabcdefhijkmnprstwxyz2345678"
)

// accountBase WPS 账号域（换 session Cookie 在这里）。变量而非常量：测试要替换。
var accountBase = accountBaseDefault

// 扫码会话状态（前端据此渲染）。
const (
	QRStatePending   = "pending"   // 二维码已就绪，等待扫码
	QRStateScanned   = "scanned"   // 已扫码，等待手机确认
	QRStateSuccess   = "success"   // 已拿到 wps_sid 并落盘
	QRStateFailed    = "failed"    // 失败（含超时）
	QRStateCancelled = "cancelled" // 用户取消
)

// QRSession 一次扫码登录会话。
type QRSession struct {
	cancel context.CancelFunc

	mu     sync.Mutex
	state  string
	qrURL  string
	err    error
	hint   string
	result Result
}

// Snapshot 供 HTTP 状态接口序列化。
func (s *QRSession) Snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]any{"state": s.state, "qr": s.qrURL}
	if s.err != nil {
		out["error"] = s.err.Error()
	}
	if s.hint != "" {
		out["hint"] = s.hint
	}
	if s.state == QRStateSuccess {
		out["uid"] = s.result.UID
		out["nickname"] = s.result.Nickname
		if s.result.Warn != "" {
			out["warning"] = s.result.Warn
		}
	}
	return out
}

// State 当前状态。
func (s *QRSession) State() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Cancel 取消会话。
func (s *QRSession) Cancel() { s.set(QRStateCancelled, nil, "") }

func (s *QRSession) set(state string, err error, hint string) {
	s.mu.Lock()
	s.state = state
	s.err = err
	s.hint = hint
	s.mu.Unlock()

	s.cancel()
}

func (s *QRSession) setQR(u string) {
	s.mu.Lock()
	s.qrURL = u
	s.mu.Unlock()
}

// resetPending 换新二维码时把会话复位成「等待扫码」。
//
// 为什么必须有：换新之后旧扫码结果作废，面板若还显示上一张码的「已扫码，请确认」，
// 用户会一直等一个永远不会到达的确认（且他扫的是已经作废的那张）。
func (s *QRSession) resetPending(qrURL string) {
	s.mu.Lock()
	s.qrURL = qrURL
	s.state = QRStatePending
	s.hint = "请用手机 WPS 扫码登录"
	s.mu.Unlock()
}

func (s *QRSession) succeed(r Result) {
	s.mu.Lock()
	s.result = r
	s.state = QRStateSuccess
	s.err = nil
	s.hint = ""
	s.mu.Unlock()
}

// StartQR 申请二维码并在后台推进扫码流程。
//
// onFound 与手工粘贴路径共用：拿到 wps_sid 后校验 + 落盘 + 重载账号池，
// 返回 error 表示这个 sid 不可用（不该发生，扫码拿到的就是登录态）。
func StartQR(onFound func(sid string) (Result, error)) (*QRSession, error) {
	ctx, cancel := context.WithTimeout(context.Background(), qrTimeout+30*time.Second)
	s := &QRSession{cancel: cancel, state: QRStatePending}

	q, err := newQRHTTP()
	if err != nil {
		cancel()
		return nil, err
	}
	// PKCE：与登录页一致（非 WPS 客户端分支一律 generatePKCEParam）。
	// code_challenge 在申请二维码时就上报，code_verifier 只在 grant_token /
	// loginCallbackApp 分支使用 —— 与网页版逐参数对齐，避免上游「该 loginid 没绑 verifier」。
	verifier, challenge := newPKCEPair()

	loginID, qrURL, err := newQRLogin(ctx, q, challenge)
	if err != nil {
		cancel()
		return nil, err
	}
	s.setQR(qrURL)
	log.Printf("comate 扫码登录：二维码已生成 loginid=%s", loginID)

	go s.run(ctx, q, loginID, verifier, onFound)
	return s, nil
}

// run 长轮询扫码状态。
func (s *QRSession) run(ctx context.Context, q *qrHTTP, loginID, verifier string, onFound func(string) (Result, error)) {
	defer s.cancel()

	state := "scan"
	refreshed := time.Now()
	lastErr := ""

	for {
		select {
		case <-ctx.Done():
			if s.State() == QRStatePending || s.State() == QRStateScanned || s.State() == QRStateCancelled {
				s.set(QRStateFailed, errors.New("扫码登录超时（5 分钟）：二维码已失效，请重新获取"), "")
			}
			return
		default:
		}

		// 主动换新二维码：上游 TTL 约 2 分钟。
		//
		// **只在「还没被扫」时换**：一旦用户扫了（state 已切到 confirm），换新等于把这次扫码
		// 作废。实测事故：10:32:37.388 记「已扫码」，86ms 后就把二维码换成新的，
		// 用户的确认落在旧 loginid 上，会话既不报错也不前进，面板永远停在「已扫码，请确认」。
		if state == "scan" && time.Since(refreshed) > qrRefreshGap {
			if id, u, err := newQRLogin(ctx, q, challengeOf(verifier)); err == nil {
				loginID, refreshed = id, time.Now()
				state = "scan"
				s.resetPending(u)
				log.Printf("comate 扫码登录：二维码已刷新 loginid=%s", id)
			}
		}

		resp, err := pollQR(ctx, q, loginID, state)
		if err != nil {
			if ctx.Err() != nil {
				continue
			}
			msg := err.Error()
			if msg != lastErr {
				lastErr = msg
				log.Printf("comate 扫码登录：轮询失败: %v", err)
			}
			time.Sleep(time.Second)
			continue
		}

		if resp.Result != "" && resp.Result != "ok" {
			// QRCodeExpired / InvalidQRLoginId：换一张继续等。
			if id, u, err := newQRLogin(ctx, q, challengeOf(verifier)); err == nil {
				loginID, refreshed = id, time.Now()
				state = "scan"
				s.resetPending(u)
				log.Printf("comate 扫码登录：二维码过期，已换新 loginid=%s", id)
				continue
			}
			s.set(QRStateFailed, fmt.Errorf("获取登录二维码失败：%s", resp.Result), "")
			return
		}

		switch resp.State {
		case "scan":
			// 已扫码：切到 confirm 继续轮询。
			if s.State() != QRStateScanned {
				s.mu.Lock()
				s.state = QRStateScanned
				s.hint = "已扫码，请在手机 WPS 上确认登录"
				s.mu.Unlock()
				log.Printf("comate 扫码登录：已扫码（nickname=%s），等待手机确认", resp.Nickname)
			}
			state = "confirm"
		case "logined":
			sid, xerr := exchangeSession(ctx, q, resp, verifier)
			if xerr != nil {
				s.set(QRStateFailed, xerr, "")
				log.Printf("comate 扫码登录：%v", xerr)
				return
			}
			r, ferr := onFound(sid)
			if ferr != nil {
				s.set(QRStateFailed, ferr, "")
				log.Printf("comate 扫码登录：账号落盘失败: %v", ferr)
				return
			}
			s.succeed(r)
			log.Printf("comate 扫码登录：成功 uid=%s", r.UID)
			return
		}
	}
}

// qrLoginResp 长轮询响应（字段按实测裁剪）。
type qrLoginResp struct {
	Result      string `json:"result"`
	State       string `json:"state"`
	Msg         string `json:"msg"`
	Nickname    string `json:"nickname"`
	Avatar      string `json:"avatar"`
	AuthCode    string `json:"authcode"`
	KsoAuthCode string `json:"kso_authcode"`
}

// newQRLogin 申请一个二维码，返回 loginid 与图片地址。
//
// codeChallenge 必须随申请一起上报（登录页在非 WPS 客户端分支就是这么做的）：
// 上游据此把 PKCE verifier 绑到这个 loginid 上，后续 grant_token 才认 code_verifier。
func newQRLogin(ctx context.Context, q *qrHTTP, codeChallenge string) (string, string, error) {
	first := url.Values{}
	first.Set("_jsonp", "cb")
	if codeChallenge != "" {
		first.Set("code_challenge", codeChallenge)
	}
	raw, err := q.get(ctx, qrAPIBase+"/api/v3/login_qrcode?"+first.Encode(), accountBase+"/")
	if err != nil {
		return "", "", err
	}
	var head struct {
		LoginID string `json:"loginid"`
	}
	if err := json.Unmarshal([]byte(jsonpBody(raw)), &head); err != nil || head.LoginID == "" {
		return "", "", fmt.Errorf("申请登录二维码失败：响应无法解析（%s）", truncateStr(raw, 120))
	}

	vals := url.Values{}
	vals.Set("loginid", head.LoginID)
	vals.Set("data", "{}")
	raw, err = q.get(ctx, qrAPIBase+"/api/v3/login_qrcode/url?"+vals.Encode(), accountBase+"/")
	if err != nil {
		return "", "", err
	}
	var second struct {
		Result string `json:"result"`
		URL    string `json:"url"`
	}
	if err := json.Unmarshal([]byte(jsonpBody(raw)), &second); err != nil || second.URL == "" {
		return "", "", fmt.Errorf("获取二维码图片失败：响应无法解析（%s）", truncateStr(raw, 120))
	}
	return head.LoginID, second.URL, nil
}

// pollQR 长轮询一次扫码状态。
func pollQR(ctx context.Context, q *qrHTTP, loginID, state string) (qrLoginResp, error) {
	vals := url.Values{}
	vals.Set("loginid", loginID)
	vals.Set("state", state)
	vals.Set("_jsonp", "cb")
	cctx, cancel := context.WithTimeout(ctx, qrPollTimeout)
	defer cancel()
	raw, err := q.get(cctx, qrAPIBase+"/api/v3/login_qrcode/login?"+vals.Encode(), accountBase+"/")
	if err != nil {
		return qrLoginResp{}, err
	}
	var resp qrLoginResp
	if err := json.Unmarshal([]byte(jsonpBody(raw)), &resp); err != nil {
		return qrLoginResp{}, fmt.Errorf("扫码状态响应无法解析（%s）", truncateStr(raw, 120))
	}
	return resp, nil
}

// exchangeSession 用一次性 authcode 换取会话 Cookie，返回 wps_sid。
//
// 网页版登录页在拿到 authcode 后按客户端类型分岔（WPS 客户端 / kso 客户端 / 普通网页），
// 这里把三条都试一遍，任一步拿到 wps_sid 即成功：
//
//	① POST /api/session/exchange/login   {authcode}            —— 普通网页主路径
//	② GET  /loginCallbackApp?authcode=…&ksoauthcode=…&codeverifier=… —— kso 客户端路径
//	③ POST /passport/secure/api/grant_token {code: kso_authcode, code_verifier} —— kso 码换票
//
// ⚠️ 三条**都必须带 CSRF**（Cookie `csrf` + 头 `X-CSRFToken`，见 newQRHTTP）：
// 缺它 ① 直接 403 `csrfRequire`，日志却只说「上游未下发 wps_sid」，R137 前就是这样
// 白扫了两次码。失败时把每条分支的响应体（截断）拼进错误里，别再让响应体陪葬。
func exchangeSession(ctx context.Context, q *qrHTTP, resp qrLoginResp, verifier string) (string, error) {
	jctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// 只记长度不记值：authcode 是一次性凭据，不该进日志（不变量 2 同款纪律）。
	log.Printf("comate 扫码登录：已确认（authcode %d 位、kso_authcode %d 位）",
		len(resp.AuthCode), len(resp.KsoAuthCode))

	failures := make([]string, 0, 3)
	note := func(stage, detail string) {
		failures = append(failures, stage+": "+truncateStr(detail, 160))
	}

	if resp.AuthCode != "" {
		body, _ := json.Marshal(map[string]string{"authcode": resp.AuthCode})
		raw, err := q.postJSON(jctx, accountBase+"/api/session/exchange/login", body)
		switch {
		case err != nil:
			note("exchange/login", err.Error())
		default:
			if sid := cookieValue(q.cli, CookieName); sid != "" {
				return sid, nil
			}
			note("exchange/login", raw)
		}

		// 兼容路径：整页导航（跟随 302），kso 客户端走的就是这一支。
		v := url.Values{}
		v.Set("authcode", resp.AuthCode)
		v.Set("logintype", "qr")
		v.Set("codeverifier", verifier)
		if resp.KsoAuthCode != "" {
			v.Set("ksoauthcode", resp.KsoAuthCode)
		}
		raw, err = q.get(jctx, accountBase+"/loginCallbackApp?"+v.Encode(), accountBase+"/login")
		switch {
		case err != nil:
			note("loginCallbackApp", err.Error())
		default:
			if sid := cookieValue(q.cli, CookieName); sid != "" {
				return sid, nil
			}
			note("loginCallbackApp", fmt.Sprintf("页面未下发 Cookie（%d 字节）", len(raw)))
		}
	}

	if resp.KsoAuthCode != "" {
		body, _ := json.Marshal(map[string]any{
			"grant_type":     "authorization_code",
			"code":           resp.KsoAuthCode,
			"code_verifier":  verifier,
			"is_append":      false,
			"login_scenario": "qr",
			// getSlv() 在无 WebCrypto 的浏览器里回 "none"，届时登录页只带 slv 不签名；
			// 服务端没有 ksoAccount 私钥，只能走这一支（若上游坚持要签名会明说）。
			"slv": "none",
		})
		raw, err := q.postJSON(jctx, accountBase+"/passport/secure/api/grant_token", body)
		switch {
		case err != nil:
			note("grant_token", err.Error())
		default:
			if sid := cookieValue(q.cli, CookieName); sid != "" {
				return sid, nil
			}
			note("grant_token", raw)
		}
	}

	detail := strings.Join(failures, "；")
	log.Printf("comate 扫码登录：换取登录态失败，逐条原因: %s", detail)
	return "", fmt.Errorf("扫码已确认，但换取登录态失败（%s）。请改用「手工方式」粘贴一次", detail)
}

// cookieValue 从会话 Cookie 罐里取指定 Cookie 的值。
func cookieValue(cli *http.Client, name string) string {
	if cli.Jar == nil {
		return ""
	}
	for _, u := range []string{accountBase + "/", "https://www.wps.cn/", qrAPIBase + "/"} {
		parsed, err := url.Parse(u)
		if err != nil {
			continue
		}
		for _, c := range cli.Jar.Cookies(parsed) {
			if c.Name == name && c.Value != "" {
				return c.Value
			}
		}
	}
	return ""
}

// qrHTTP 扫码流程共用的 HTTP 客户端：带 Cookie 罐与 CSRF 双提交。
//
// CSRF 是「双提交」形态（见登录页 common.createCsrftoken / getCsrftoken）：
// 同一个 32 位随机串既写进 Cookie `csrf`（域 .wps.cn，account/qr 两个子域共用），
// 又写进请求头 `X-CSRFToken`；上游只校验两者存在且一致。
type qrHTTP struct {
	cli  *http.Client
	csrf string
}

func newQRHTTP() (*qrHTTP, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	csrf := newCSRFToken()
	// 注：cookie 不在这里预置 —— 见 ensureCSRF。登录页把 csrf 写成 wps.cn 的域 cookie，
	// 而这里按「实际请求的 host」逐次落 host-only cookie，等价且不依赖域名匹配
	// （域名 cookie 遇到 httptest 的 127.0.0.1 会被 cookie jar 直接丢掉，测不出来）。
	return &qrHTTP{cli: &http.Client{Jar: jar}, csrf: csrf}, nil
}

// ensureCSRF 保证目标 host 上带着 csrf cookie（双提交的另一半）。
func (q *qrHTTP) ensureCSRF(u *url.URL) {
	for _, c := range q.cli.Jar.Cookies(u) {
		if c.Name == csrfCookieName && c.Value != "" {
			return
		}
	}
	q.cli.Jar.SetCookies(u, []*http.Cookie{{Name: csrfCookieName, Value: q.csrf, Path: "/"}})
}

func (q *qrHTTP) get(ctx context.Context, rawURL, referer string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	q.ensureCSRF(req.URL)
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Referer", referer)
	req.Header.Set("Accept", "*/*")
	resp, err := q.cli.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	return string(body), nil
}

// postJSON 发一个 JSON POST（带 CSRF 头；返回响应体供调用方判断失败原因）。
func (q *qrHTTP) postJSON(ctx context.Context, rawURL string, payload []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(string(payload)))
	if err != nil {
		return "", err
	}
	q.ensureCSRF(req.URL)
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Referer", accountBase+"/login")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(csrfHeader, q.csrf)
	resp, err := q.cli.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return string(body), nil
}

// jsonpBody 剥掉 `cb(...)` 外壳；不是 JSONP 形态则原样返回。
func jsonpBody(s string) string {
	s = strings.TrimSpace(s)
	i := strings.Index(s, "(")
	j := strings.LastIndex(s, ")")
	if i > 0 && j > i && strings.HasSuffix(strings.TrimSpace(s[:i]), "cb") {
		return s[i+1 : j]
	}
	return s
}

// browserUA 与网页版登录页一致的 UA（上游对空 UA 会走不同的分支）。
const browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"

// truncateStr 截断用于错误信息。
func truncateStr(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
