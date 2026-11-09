// browser.go WPS Comate「用本机浏览器登录」——接管一台**已经在跑**的浏览器。
//
// 三条登录路径的分工（同一份凭据纪律，不同部署形态）：
//
//	手工粘贴   —— 任何环境都能用，但要求用户会开 F12 找 wps_sid，门槛最高；
//	扫码登录   —— 服务端自己申请二维码，无桌面也能用（NAS 默认路径）；
//	本机浏览器 —— 用户在自己熟悉、已登录（或多账号来回切）的 Chrome/Edge 里
//	              点一下链接完成登录，wild-work 立刻从那个浏览器读到 wps_sid。
//
// 第三条路与 internal/cdp 的 Launch 完全相反：**不新开浏览器、不用临时 profile**。
// 它用 cdp.Attach 接管用户已有的调试端口，因此读到的就是用户天天在用的那个
// 浏览器的登录态，登录一次即可；代价是必须由用户以 --remote-debugging-port
// 启动浏览器（面板里写清楚怎么开）。
//
// 安全边界：DELETE 一律不碰用户的浏览器 —— 会话结束时只解引用（cdp.Browser.attached）。
package logincomate

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"wild-work/internal/cdp"
)

const (
	// browserLoginURL 用户要登录的页面（与扫码路径同一套账号体系）。
	browserLoginURL = "https://account.wps.cn/login"
	// browserPollInterval 轮询 Cookie 的间隔：登录完成后最多等这么久就能落盘。
	browserPollInterval = 2 * time.Second
	// browserTimeout 整个「去浏览器里登录」流程的最长等待。
	browserTimeout = 10 * time.Minute
)

// BrowserSession 一次「接管本机浏览器登录」会话。
type BrowserSession struct {
	cancel context.CancelFunc

	mu     sync.Mutex
	state  string
	err    error
	hint   string
	result Result
}

// Snapshot 供 HTTP 状态接口序列化。
func (s *BrowserSession) Snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]any{"state": s.state}
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
func (s *BrowserSession) State() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Cancel 取消会话（不关闭用户的浏览器）。
func (s *BrowserSession) Cancel() { s.set(QRStateCancelled, nil, "") }

func (s *BrowserSession) set(state string, err error, hint string) {
	s.mu.Lock()
	s.state = state
	s.err = err
	s.hint = hint
	s.mu.Unlock()
	s.cancel()
}

func (s *BrowserSession) update(format string, args ...any) {
	hint := format
	if len(args) > 0 {
		hint = fmt.Sprintf(format, args...)
	}
	s.mu.Lock()
	s.hint = hint
	s.mu.Unlock()
}

func (s *BrowserSession) succeed(r Result) {
	s.mu.Lock()
	s.result = r
	s.state = QRStateSuccess
	s.err = nil
	s.hint = ""
	s.mu.Unlock()
	s.cancel()
}

// StartBrowserLogin 接管 endpoint（http://127.0.0.1:9222 形式）指向的浏览器，
// 开一个新标签页到 WPS 登录页，然后轮询该浏览器的 Cookie。
//
// onFound 与扫码/手工路径共用（校验 + 落盘 + 重载账号池）。
func StartBrowserLogin(endpoint string, onFound func(sid string) (Result, error)) (*BrowserSession, error) {
	ep := strings.TrimSpace(endpoint)
	if ep == "" {
		return nil, fmt.Errorf("未配置本机浏览器调试地址（login.comate_browser_cdp），例如 http://127.0.0.1:9222")
	}

	ctx, cancel := context.WithTimeout(context.Background(), browserTimeout)
	s := &BrowserSession{cancel: cancel, state: QRStatePending}

	browser, err := cdp.Attach(ctx, ep)
	if err != nil {
		cancel()
		return nil, err
	}

	// 开标签页失败不算致命：用户可能已经手动打开了登录页。
	if _, oerr := browser.OpenTab(ctx, browserLoginURL); oerr != nil {
		log.Printf("comate browser login: open tab failed endpoint=%s err=%v", ep, oerr)
		s.update("已连接浏览器；如果没看到登录页，请手动打开 " + browserLoginURL)
	} else {
		s.update("已在你打开的浏览器里打开 WPS 登录页，登录完成后会自动读取")
	}

	go func() {
		defer cancel()
		// 注意：cdp.Browser.attached ⇒ Close 只解引用，不会关掉用户的浏览器。
		defer browser.Close()

		baseline := ""
		if cookies, err := browser.AllCookies(ctx, "wps.cn"); err == nil {
			baseline = sidOf(cookies)
		}

		ticker := time.NewTicker(browserPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				if s.State() != QRStateSuccess && s.State() != QRStateCancelled {
					s.set(QRStateFailed, fmt.Errorf("等待登录超时（%s）", browserTimeout), "")
				}
				return
			case <-ticker.C:
			}

			cookies, err := browser.AllCookies(ctx, "wps.cn")
			if err != nil {
				s.update("已在浏览器登录页；等待登录完成（读取 Cookie 失败：%v）", err)
				continue
			}
			sid := sidOf(cookies)
			if sid == "" {
				s.update("请在浏览器里登录 WPS 账号…")
				continue
			}
			// 与开跑前的值相同 ⇒ 还是那个已登录的账号，没必要反复校验；
			// 用户切号后 Cookie 会变，那时再校验。
			if sid == baseline && s.State() == QRStatePending {
				s.update("检测到浏览器里已有登录态，正在校验该账号…")
			}
			res, ferr := onFound(sid)
			if ferr != nil {
				// 未登录态也可能有预登录 Cookie：校验不过就继续等，不当失败。
				s.update("等待浏览器登录完成（当前凭据无效：%v）", ferr)
				baseline = sid
				continue
			}
			s.succeed(res)
			return
		}
	}()

	return s, nil
}

// sidOf 从 Cookie 列表里取 wps_sid。
func sidOf(cookies []cdp.Cookie) string {
	for _, c := range cookies {
		if c.Name == CookieName && strings.TrimSpace(c.Value) != "" &&
			(c.Domain == "" || strings.Contains(c.Domain, "wps.cn")) {
			return strings.TrimSpace(c.Value)
		}
	}
	return ""
}
