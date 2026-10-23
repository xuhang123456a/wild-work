//go:build live

// live_probe_test.go 小浣熊（raccoon）真实账号探针 —— 阶段 B4 未验证项。
//
// 背景：`本地 docs/loomy-raccoon接入记录.md` §9 列了 4 项「仍待验证」，本探针处理其中
// **可验证**的一项，并用实测结果回答另一个长期争议问题：
//
//	【争议】refresh_token 到底是「单会话」（两边共用一份即互踢）还是「各自可并存」？
//	        文档原写「导入后必须退出官方客户端」，但用户实测「两边同时在线均正常」。
//
// 本探针回答三件事：
//
//  1. **refresh 真实轮换的成功路径**（§9 第 7 项）：wild-work 独占时调 /refresh，
//     是否真的换发新 access_token + 新 refresh_token？新 access 能否立即用于推理？
//  2. **轮换后旧 refresh_token 是否失效**（决定「单会话」的真实含义）：
//     用轮换前的旧 token 再刷一次 —— 若成功则是「多会话共存」（新旧都有效）；
//     若报 refresh_conflict/reuse 则是「单会话轮换」（旧 token 被作废）。
//  3. **access_token 的滑动策略**：刷新后旧 access 是否仍可用（补充 §9 第 4 项）。
//
// ⚠️ 本探针**会修改账号文件**（refresh 后落盘新 token），这是它的验证目标本身。
// 运行前请自行备份 auths/raccoon-*.json；探针结束会打印新旧 token 的哈希对比。
//
// 本文件带 `live` build tag，**默认不参与编译**，不改任何生产路径。
//
// ⚠️ 运行前必须确认：**没有正在运行的 wild-work 实例使用同一个 auths 目录**。
// 探针会轮换 refresh_token 并落盘，运行中的实例内存里还是旧 token，
// 会因 401 被标 session_dead 禁用（2026-09-25 实际发生过）。要么先停实例，要么用副本目录。
//
// 运行方式（仓库根目录）：
//
//	WILDWORK_AUTHDIR=~/Downloads/auths go test -tags live ./internal/raccoon/ -run TestLiveProbeRefresh -v -timeout 600s
//
// 可调环境变量：
//
//	WILDWORK_AUTHDIR  账号目录（默认 ./auths），读取 raccoon-*.json
package raccoon

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
)

// sidOf 取 refresh_token 的 JWT `sid` 声明哈希（判断是否同一会话；不泄露原值）。
func sidOf(t *testing.T, tok string) string {
	t.Helper()
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return "(非JWT)"
	}
	p := parts[1]
	if m := len(p) % 4; m != 0 {
		p += strings.Repeat("=", 4-m)
	}
	raw, err := base64.URLEncoding.DecodeString(p)
	if err != nil {
		return "(解码失败)"
	}
	var claims struct {
		SID string `json:"sid"`
		JTI string `json:"jti"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return "(解析失败)"
	}
	return shortSHA(claims.SID) + "/jti=" + shortSHA(claims.JTI)
}

// shortSHA 对值取短哈希（用于对比「是否相同」而不打印原值）。
func shortSHA(s string) string {
	if s == "" {
		return "(空)"
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:6])
}

// expOf 取 JWT 的 exp 并格式化为本地时间（不泄露 token）。
func expOf(tok string) string {
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return "(非JWT)"
	}
	p := parts[1]
	if m := len(p) % 4; m != 0 {
		p += strings.Repeat("=", 4-m)
	}
	raw, err := base64.URLEncoding.DecodeString(p)
	if err != nil {
		return "(解码失败)"
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil || claims.Exp == 0 {
		return "(无 exp)"
	}
	return time.Unix(claims.Exp, 0).Format("2006-01-02 15:04:05")
}

// loadRaccoonAuth 从 authDir 读第一个 raccoon 账号（失败即 Skip，不误报）。
func loadRaccoonAuth(t *testing.T) (*auth.Auth, string) {
	t.Helper()
	dir := os.Getenv("WILDWORK_AUTHDIR")
	if dir == "" {
		dir = "./auths"
	}
	dir = expandHome(dir)
	as, err := auth.LoadRaccoonDir(dir)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", dir, err)
	}
	if len(as) == 0 {
		t.Skipf("在 %s 没找到 raccoon-*.json —— 用面板导入一个账号再跑", dir)
	}
	return as[0], dir
}

// expandHome 把 ~ 展开为用户目录（Windows 下 Go 的 os.UserHomeDir 可用）。
func expandHome(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return home + p[1:]
}

// TestLiveProbeRefresh 验证 refresh 真实轮换 + 旧 token 是否失效 + 双会话是否可并存。
//
// 这是 §9 第 7 项（「小浣熊 refresh 真实轮换的成功路径」）的正式验证，同时用
// 「轮换后旧 token 能否再用」回答争议中的「单会话」真实含义。
func TestLiveProbeRefresh(t *testing.T) {
	a, dir := loadRaccoonAuth(t)
	c := New()

	oldAccess := a.AccessTokenValue()
	oldRefresh := a.RefreshTokenValue()
	if oldRefresh == "" {
		t.Skip("该账号无 refresh_token（可能是网页版一次性凭据），无法验证轮换")
	}

	t.Logf("=== 轮换前 ===")
	t.Logf("authdir          = %s", dir)
	t.Logf("uid              = %s", a.UID)
	t.Logf("access  exp      = %s", expOf(oldAccess))
	t.Logf("refresh exp      = %s", expOf(oldRefresh))
	t.Logf("refresh sid      = %s", sidOf(t, oldRefresh))

	// ---------- 1. 正常轮换 ----------
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh 失败（期望成功）: %v", err)
	}
	newAccess := a.AccessTokenValue()
	newRefresh := a.RefreshTokenValue()

	if newAccess == oldAccess {
		t.Errorf("access_token 未变化 —— 上游未轮换 access（异常）")
	} else {
		t.Logf("✅ access_token 已轮换：exp %s → %s", expOf(oldAccess), expOf(newAccess))
	}
	if newRefresh == oldRefresh {
		t.Logf("⚠️ refresh_token 未变化（exp %s）—— 该上游此路径不轮换 refresh", expOf(newRefresh))
	} else {
		t.Logf("✅ refresh_token 已轮换：exp %s → %s", expOf(oldRefresh), expOf(newRefresh))
		t.Logf("   sid：%s → %s", sidOf(t, oldRefresh), sidOf(t, newRefresh))
	}

	// 落盘（模拟生产：AGENTS §6.20 凡 refresh 必紧跟 SaveAtomic）
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic 失败: %v", err)
	}

	// ---------- 2. 新 access 能否立即用于真实推理 ----------
	body := []byte(`{"model":"raccoon-8c4485","messages":[{"role":"user","content":"只回复两个字：正常"}],"max_tokens":32,"stream":false}`)
	rc, status, _, err := c.ChatStream(a, body)
	if err != nil || status >= 400 {
		t.Errorf("轮换后的 access_token 无法推理：status=%d err=%v", status, err)
	} else {
		resp, aerr := c.Aggregate(rc, "raccoon/raccoon-8c4485")
		rc.Close()
		if aerr != nil {
			t.Errorf("聚合失败: %v", aerr)
		} else {
			t.Logf("✅ 轮换后 access 可用于推理，响应含 %d 个 choice", len(resp["choices"].([]any)))
		}
	}

	// ---------- 3. 旧 refresh_token 是否仍有效（「单会话」争议的判定点）----------
	t.Logf("=== 关键判定：旧 refresh_token 是否已作废 ===")
	probe := &auth.Auth{
		UID:          a.UID,
		AccessToken:  oldAccess,
		RefreshToken: oldRefresh,
		ExpiresAt:    a.ExpiresAt,
		ApiHost:      a.ApiHost,
		Domain:       a.Domain,
	}
	errOld := c.RefreshToken(probe)
	switch {
	case errOld == nil:
		t.Logf("→ ✅ 旧 refresh_token **仍可刷新** ⇒ 新旧并存（多会话），")
		t.Logf("   印证「导入后无需退出官方客户端」——两边各持一份即可共存")
		t.Logf("   旧 token 刷出的 access exp=%s", expOf(probe.AccessTokenValue()))
	case strings.Contains(errOld.Error(), "conflict") || strings.Contains(errOld.Error(), "reuse"):
		t.Logf("→ ⚠️ 旧 refresh_token 已被作废（%v）⇒ 单次轮换语义", errOld)
		t.Logf("   此时「两边共用同一份 token」会互踢，但**各自独立 token 仍可并存**")
	default:
		t.Logf("→ ❓ 旧 refresh_token 刷新返回其它错误：%v", errOld)
	}

	// ---------- 4. 旧 access_token 是否仍可用（补充 §9 第 4 项）----------
	rc2, status2, _, err2 := c.ChatStream(probe, body)
	if err2 != nil || status2 >= 400 {
		t.Logf("旧 access_token 已失效（status=%d）—— 刷新会作废旧 access", status2)
	} else {
		rc2.Close()
		t.Logf("旧 access_token 仍可用（status=%d）—— 刷新不作废旧 access", status2)
	}

	t.Logf("=== 结束：新凭据已落盘到 %s ===", dir)
	t.Logf("如需回滚，用运行前的 .bak 文件覆盖即可")
}
