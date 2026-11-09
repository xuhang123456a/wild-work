package loginlingxi

import (
	"testing"
	"time"
)

// TestWebLoginURL 登录入口指向 WPS 官网（首页自带登录入口；
// 不得改成其它域：wps_sid 只在 www.wps.cn 及其父域 .wps.cn 下发）。
func TestWebLoginURL(t *testing.T) {
	if WebLoginURL != "https://www.wps.cn" {
		t.Fatalf("WebLoginURL 不符：%s", WebLoginURL)
	}
}

// TestCookieConstants Cookie 名与域必须与 internal/lingxi 对齐。
func TestCookieConstants(t *testing.T) {
	if CookieName != "wps_sid" {
		t.Fatalf("CookieName 不符：%s", CookieName)
	}
	if CookieDomain != "wps.cn" {
		t.Fatalf("CookieDomain 不符：%s", CookieDomain)
	}
}

// TestPollTimeout 必须长于通用 loginTimeout（5m），否则 pollLogin 会先到、
// 把还在等用户登录的浏览器一起关掉（R97 同款教训）；
// 且须比 LoginTimeout 多出余量——恰好相等时 app 层通用文案先触发，
// 包内专门写的超时终态消息不可达（见 login.go PollTimeout 注释）。
func TestPollTimeout(t *testing.T) {
	if PollTimeout() <= LoginTimeout {
		t.Fatalf("PollTimeout 应大于 LoginTimeout（留终态观测余量），实际 %v", PollTimeout())
	}
	if PollTimeout() <= 5*time.Minute {
		t.Fatalf("PollTimeout 必须大于通用 loginTimeout(5m)，实际 %v", PollTimeout())
	}
}

// TestPollWithoutSession 无会话时 Poll 报 ErrPending 而非 panic。
func TestPollWithoutSession(t *testing.T) {
	mu.Lock()
	old := current
	current = nil
	mu.Unlock()
	defer func() { mu.Lock(); current = old; mu.Unlock() }()

	if _, err := Poll(nil, ""); err != ErrPending {
		t.Fatalf("无会话时 Poll 应返回 ErrPending（取消由上层 ctx 分支处理），实际 %v", err)
	}
}

// TestPollSessionStates 覆盖 Poll 对各状态的映射（不启动真实浏览器）。
func TestPollSessionStates(t *testing.T) {
	mk := func(status string, err error, res Result) *WebSession {
		s := &WebSession{status: status, err: err}
		if status == "success" {
			s.result = res
		}
		return s
	}
	cases := []struct {
		name    string
		sess    *WebSession
		wantErr bool
	}{
		{"pending", mk("pending", nil, Result{}), true},
		{"success", mk("success", nil, Result{SID: "sid-x", UID: "uid-y"}), false},
		{"failed", mk("failed", errStr("浏览器未找到"), Result{}), true},
		{"cancelled", mk("cancelled", nil, Result{}), true},
	}
	for _, c := range cases {
		mu.Lock()
		current = c.sess
		mu.Unlock()
		r, err := Poll(nil, "")
		if c.wantErr {
			if err == nil {
				t.Errorf("%s: 期望错误，实际成功 %+v", c.name, r)
			}
			if c.name == "pending" && err != ErrPending {
				t.Errorf("%s: 期望 ErrPending 实际 %v", c.name, err)
			}
		} else {
			if err != nil {
				t.Errorf("%s: 期望成功，实际 %v", c.name, err)
			}
			if r.SID != "sid-x" || r.UID != "uid-y" {
				t.Errorf("%s: 凭据不符 %+v", c.name, r)
			}
		}
	}
	defer func() { mu.Lock(); current = nil; mu.Unlock() }()
}

// TestStartRejectsConcurrentLogin 已有 pending 会话时拒绝再次发起。
func TestStartRejectsConcurrentLogin(t *testing.T) {
	mu.Lock()
	current = &WebSession{status: "pending"}
	mu.Unlock()
	defer func() { mu.Lock(); current = nil; mu.Unlock() }()

	if _, err := Start(nil, ""); err == nil {
		t.Fatal("已有 pending 会话时应拒绝并发登录")
	}
}

// TestShutdownIdempotent 无会话时 Shutdown 必须静默成功（幂等）。
func TestShutdownIdempotent(t *testing.T) {
	mu.Lock()
	current = nil
	mu.Unlock()
	Shutdown()
	Shutdown()
}

type errStr string

func (e errStr) Error() string { return string(e) }
