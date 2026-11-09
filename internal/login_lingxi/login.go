// Package loginlingxi 实现 WPS 灵犀（lingxi.kdocs.cn）的「网页版自动登录」。
//
// 与 login_comate 同构（登录页同为 www.wps.cn、凭据同为 wps_sid Cookie，见
// internal/login_comate 包注释的完整流程说明），仅两处实质差异：
//
//   - 验证函数：本包用 lingxi.ValidateSID（GET /api/aioffice/v1/sessions/plans，
//     灵犀侧门禁）——comate 的 ValidateSID 走 /models，而本机账号 comate 侧未
//     开通（空目录），若沿用会把「灵犀可用」的账号误判为登录未完成（双产品
//     门禁独立，见主题库 WPS渠道灵犀与comate.md）；
//   - 落盘走 App 层 persistLingxiAccount（与粘贴路径 SubmitLingxiSID 共用，
//     文件 lingxi-<uid>.json、池 state-lingxi.json，与 comate 完全隔离）。
//
// 与调用方（internal/app）的契约（与其余 login_* 包签名一致）：
//
//	Start(_ *http.Client, _ string) → 拉起浏览器并返回登录页地址，后台开始等待
//	Poll(_ *http.Client, _ string)  → 轮询结果：ErrPending / Result / 终态 error
//	Shutdown()                      → 取消并关闭浏览器（幂等）
//
// 两个用不上的参数（http client / state 文件路径）为何仍保留：本包走 CDP 捕获
// Cookie，不走 HTTP 轮询、也不落 state 文件——但**签名必须与其它包同构**，否则
// 调用方 internal/app 的 loginFlows 表驱动无法把本包当作普通一员处理。
//
// 会话状态存于包级变量（App 已用 loginBusy 保证同一时刻只有一次登录）。
package loginlingxi

import (
	"errors"
	"log"
	"net/http"
	"sync"
	"time"
)

// ErrPending 表示登录尚未完成（用户在浏览器里还没走完）。
var ErrPending = errors.New("login pending")

// 包级当前会话（App 层 loginBusy 已保证串行）。
var (
	mu      sync.Mutex
	current *WebSession
)

// Start 发起网页版登录：拉起独立 profile 的浏览器并打开 WPS 官网。
// 返回值即登录页地址（面板可展示，无需再 window.open——浏览器已由本函数拉起）。
//
// 两个参数与其它 login_* 包同构但本包不用，理由见包注释。
func Start(_ *http.Client, _ string) (string, error) {
	mu.Lock()
	if current != nil {
		if st, _ := current.Status(); st == "pending" {
			mu.Unlock()
			return "", errors.New("已有 WPS 灵犀登录流程进行中，请先在浏览器完成登录或取消")
		}
	}
	mu.Unlock()

	sess, err := StartWebLogin(nil)
	if err != nil {
		return "", err
	}
	mu.Lock()
	current = sess
	mu.Unlock()
	return WebLoginURL, nil
}

// Poll 单次检查登录结果：未完成返回 ErrPending；完成返回 Result；失败返回错误。
//
// 无会话（如已被 Shutdown 取消）时返回 ErrPending 而非硬错误——与其它渠道一致：
// 取消后由 pollLogin 的 ctx.Done() 分支给出「已取消」文案，而不是把取消报成登录失败。
//
// 参数同构但不用，理由见包注释。
func Poll(_ *http.Client, _ string) (Result, error) {
	mu.Lock()
	sess := current
	mu.Unlock()
	if sess == nil {
		return Result{}, ErrPending
	}
	status, serr := sess.Status()
	switch status {
	case "success":
		if r, ok := sess.Result(); ok {
			return r, nil
		}
		return Result{}, errors.New("登录状态异常：成功但无凭据")
	case "failed":
		if serr == nil {
			serr = errors.New("登录失败")
		}
		return Result{}, serr
	case "cancelled":
		return Result{}, errors.New("登录已取消")
	default:
		return Result{}, ErrPending
	}
}

// Shutdown 取消登录并关闭浏览器（幂等：无会话、已结束都不报错）。
func Shutdown() {
	mu.Lock()
	sess := current
	current = nil
	mu.Unlock()
	if sess == nil {
		return
	}
	if st, _ := sess.Status(); st == "pending" {
		sess.Cancel()
		log.Printf("lingxi 网页版登录：已取消并关闭浏览器")
	}
}

// PollTimeout 供 loginFlows 表的 pollTimeout 字段使用。
//
// CDP 等待上限是 10 分钟，而通用 loginTimeout 只有 5 分钟——不覆盖的话
// pollLogin 会先到、把还在等用户登录的浏览器连同流程一起关掉（R97 同款教训）。
//
// 取 LoginTimeout + 30s 余量而不是恰好相等：pollLogin 每轮先查 deadline 再 Poll，
// 而会话自身的 failed 终态要等最后一轮 currentSID + 2s sleep 走完才落地——
// 两个 deadline 严格相等时，几乎总是 app 层的通用文案先触发，web.go 里专门
// 写的「捕获到 wps_sid 但验证未通过…」终态消息反而不可达。留 30s 让会话
// 自己的终态先被 Poll 观测到（与 login_comate 逐字同理由）。
func PollTimeout() time.Duration { return LoginTimeout + 30*time.Second }
