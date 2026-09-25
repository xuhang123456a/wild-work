// unavailable_message_test.go 「账号全部不可用」文案回归测试。
// 历史缺陷：该文案恒拼「需重新登录（refresh token is invalid）」，
// 把超时冷却、限流等误报成凭证失效，把用户引向错误排查方向。
package server

import (
	"strings"
	"testing"
	"time"

	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// TestUnavailableAccountsMessageDoesNotFalselyDemandRelogin
// 冷却原因是连续错误（超时）时，不得提示「需重新登录」，且要带上真实原因与截止时间。
func TestUnavailableAccountsMessageDoesNotFalselyDemandRelogin(t *testing.T) {
	sts := []pool.Status{{
		UID:     "u1",
		Cooling: true,
		Until:   time.Date(2026, 9, 21, 22, 2, 31, 0, time.UTC),
		Reason:  "consecutive errors",
	}}
	msg := unavailableAccountsMessage(provider.WorkBuddy, sts, 0, 1)

	if strings.Contains(msg, "refresh token is invalid") || strings.Contains(msg, "需重新登录") {
		t.Errorf("连续错误冷却不得提示重新登录: %s", msg)
	}
	if !strings.Contains(msg, "consecutive errors") {
		t.Errorf("应带上真实冷却原因: %s", msg)
	}
	if !strings.Contains(msg, "2026-09-21T22:02:31Z") {
		t.Errorf("应带上冷却截止时间: %s", msg)
	}
}

// TestUnavailableAccountsMessageDemandsReloginOnSessionDead
// 只有真正 session dead 时才提示重新登录。
func TestUnavailableAccountsMessageDemandsReloginOnSessionDead(t *testing.T) {
	sts := []pool.Status{{UID: "u1", Disabled: true, Reason: "session dead"}}
	msg := unavailableAccountsMessage(provider.WorkBuddy, sts, 1, 0)

	if !strings.Contains(msg, "需重新登录") {
		t.Errorf("session dead 必须提示重新登录: %s", msg)
	}
}
