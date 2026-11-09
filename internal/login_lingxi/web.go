// web.go WPS 灵犀「自动获取登录态」流程（与 login_comate/web.go 同款范式）。
//
// 背景：灵犀的凭据是 WPS 账号在 www.wps.cn 下的 wps_sid Cookie（与 comate 同源，
// 但两个产品门禁独立）。官方无编程式登录接口，两条获取路径：手工（浏览器 F12
// 复制粘贴，见 App.SubmitLingxiSID）与本文件的自动路径：
//
//	① 拉起一个独立 profile 的 Edge/Chrome（带 CDP 调试端口，不污染用户浏览器）
//	② 浏览器打开 www.wps.cn，用户在真实页面里正常登录 WPS 账号
//	③ 后台循环经 CDP 读取 Cookie，wps_sid 出现且能通过 lingxi.ValidateSID 即捕获
//	④ 结果交由 App 层落盘（completeLingxiLogin），浏览器关闭、临时 profile 清理
//
// 为何验证不过也算数地继续等：www.wps.cn 对未登录访客也可能下发占位 wps_sid，
// 「Cookie 出现 ≠ 登录完成」（对齐智谱清言 R28、小浣熊访客凭据的教训），故每轮
// 都拿 ValidateSID 复核，验证失败只记 lastErr、继续等用户真正完成登录。
//
// 与 login_comate 的关键差异：验证走 **lingxi**.ValidateSID（GET plans，灵犀侧
// 门禁），不能用 comate.ValidateSID——本机账号 comate 侧未开通（/models 空目录），
// comate 验证会把灵犀可用的 wps_sid 永远判为「未完成登录」，超时收场。
package loginlingxi

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"wild-work/internal/cdp"
	"wild-work/internal/lingxi"
)

// WebLoginURL 登录页地址（StartURL；WPS 官网首页自带登录入口）。
const WebLoginURL = "https://www.wps.cn"

// CookieName 灵犀凭据所在的 Cookie 名（与 internal/lingxi 的 CookieName 对齐）。
const CookieName = "wps_sid"

// CookieDomain Cookie 域匹配片段（实际下发域为 .wps.cn）。
const CookieDomain = "wps.cn"

// LoginTimeout 等待用户完成登录的最长时间。
const LoginTimeout = 10 * time.Minute

// Result 登录成功后的账号信息（落盘由 App 层完成，本包不写文件）。
type Result struct {
	// SID 捕获到的 wps_sid（App 层落盘为 Auth.AccessToken）。
	SID string
	// UID lingxi.ValidateSID 派生的稳定账号 ID（lxsid-<sha256 前 16 位>；
	// 前缀区别于 comate 的 sid-<hash>，防双渠道同 sid 撞键串账号）。
	UID string
}

// WebSession 一次自动登录会话。
type WebSession struct {
	browser *cdp.Browser
	cancel  context.CancelFunc
	mu      sync.Mutex
	status  string // pending / success / failed / cancelled
	err     error
	result  Result
}

// Status 返回当前状态与伴随错误。
func (s *WebSession) Status() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, s.err
}

// Result 仅在 success 状态下返回登录结果。
func (s *WebSession) Result() (Result, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status != "success" {
		return Result{}, false
	}
	return s.result, true
}

// setStatus 更新状态（goroutine 与调用方并发安全）。
func (s *WebSession) setStatus(status string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
	s.err = err
}

// Cancel 用户取消：置状态并中止 ctx（浏览器随后由 defer 链关闭）。
func (s *WebSession) Cancel() {
	s.setStatus("cancelled", nil)
	s.cancel()
}

// StartWebLogin 拉起独立 profile 的浏览器打开 WPS 官网，开始等待用户登录。
//
// onSaved 在捕获并验证成功时同步回调（可为 nil：loginFlows 路径不使用它，
// 落盘由 App 层在 Poll 成功后统一执行，避免双写）。
func StartWebLogin(onSaved func(Result)) (*WebSession, error) {
	// +2 分钟冗余：给浏览器启动、CDP 就绪、以及超时后的清理留余量，
	// 不让 ctx 在等待循环中途被先到斩断（与小浣熊/智谱同款）。
	ctx, cancel := context.WithTimeout(context.Background(), LoginTimeout+2*time.Minute)
	browser, err := cdp.Launch(ctx, cdp.Options{StartURL: WebLoginURL})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("启动浏览器失败：%w", err)
	}
	sess := &WebSession{browser: browser, cancel: cancel, status: "pending"}
	go func() {
		defer cancel()
		defer func() {
			// 清理失败只记日志：会话已结束，不应因清理报错打断流程。
			if cerr := browser.Close(); cerr != nil {
				log.Printf("lingxi 自动登录：关闭浏览器失败：%v", cerr)
			}
		}()
		waitForLogin(ctx, sess, onSaved)
	}()
	return sess, nil
}

// waitForLogin 主等待循环：轮询 Cookie → 验证 → 置终态。
func waitForLogin(ctx context.Context, sess *WebSession, onSaved func(Result)) {
	deadline := time.Now().Add(LoginTimeout)
	lastSID := ""
	var lastErr error
	sawSID := false
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			sess.setStatus("cancelled", nil)
			return
		default:
		}
		sid, rerr := currentSID(ctx, sess.browser)
		if rerr != nil {
			// CDP 读取失败（用户手关浏览器最常见）：记入 lastErr，超时终态才有线索。
			lastErr = rerr
		} else if sid != "" && sid != lastSID {
			lastSID = sid
			uid, verr := lingxi.ValidateSID(sid)
			if verr == nil {
				r := Result{SID: sid, UID: uid}
				sess.mu.Lock()
				sess.result, sess.status = r, "success"
				sess.mu.Unlock()
				if onSaved != nil {
					onSaved(r)
				}
				return
			}
			lastErr = verr
			if !sawSID {
				sawSID = true
				log.Printf("lingxi 自动登录：捕获到 wps_sid 但验证未通过（可能尚未完成登录），继续等待")
			}
		}
		select {
		case <-ctx.Done():
			sess.setStatus("cancelled", nil)
			return
		case <-time.After(2 * time.Second):
		}
	}
	// 超时：优先给「见过 sid 但没通过验证」这一最有指导性的提示。
	msg := "等待登录超时"
	if sawSID {
		msg = "等待登录超时：捕获到 wps_sid 但验证未通过。请确认已在弹出的浏览器窗口中完成 WPS 账号登录后重试"
	} else if lastErr != nil {
		msg = fmt.Sprintf("等待登录超时：%v", lastErr)
	}
	sess.setStatus("failed", errors.New(msg))
}

// currentSID 读取当前浏览器里非空的 wps_sid（未出现返回 ""；读失败返回错误，
// 调用方记入 lastErr 供超时终态引用）。
func currentSID(ctx context.Context, b *cdp.Browser) (string, error) {
	cookies, err := b.AllCookies(ctx, CookieDomain)
	if err != nil {
		return "", err
	}
	for _, c := range cookies {
		if c.Name == CookieName && strings.TrimSpace(c.Value) != "" && strings.Contains(c.Domain, CookieDomain) {
			return strings.TrimSpace(c.Value), nil
		}
	}
	return "", nil
}
