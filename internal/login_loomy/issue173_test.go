package login_loomy

// issue #173 的回归护栏：Loomy 短信验证码登录「提交提示先点获取验证码，再点显示失败请重试」。
//
// 定位到的两个确定性缺陷与对应锁死点：
//
//	A. 下发验证码失败被置成**终态**，于是后续真正的登录成功被 `login` 的终态守卫静默丢弃：
//	   引导页回「登录成功」，而面板 Poll 恒报旧的「下发失败」，两边说法相反且无法自愈。
//	   ⇒ 下发失败改为「非终态、可就地重试」（session.noteSendFail），并允许被后续成功覆盖。
//	B. msgid 只存活在引导页的 JS 变量里，服务端不保存 ⇒ 页面一刷新就丢，提交必然命中
//	   「请先点「获取验证码」」，用户只好再发一次短信（还会撞上发送间隔限制）→ 回到缺陷 A。
//	   ⇒ 服务端保留最近一次 msgid（仅 loopback 引导页可见），/status 回传 + 提交时兜底补上。
//
// 另有一条同源风险：面板 Poll 已因失败/取消而 Shutdown（cur 置 nil）后，引导页标签页还开着，
// 用户再提交会真的换到一个可用 session，却再没人取走落盘 ⇒ 页面说成功、账号没入库。
// handleLogin 现在会当场拒绝并给出可执行的下一步。
//
// 全部走本机假上游（见 fakeUpstream 的注释与 AGENTS.md §0.1）：绝不真发短信。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// startGuide 起一条流程，返回引导页 base 与 statePath。
func startGuide(t *testing.T) (base, statePath string) {
	t.Helper()
	statePath = isolate(t)
	u, err := Start(NewClient(), statePath)
	if err != nil {
		t.Fatalf("Start 失败：%v", err)
	}
	t.Cleanup(Shutdown)
	return strings.TrimSuffix(u, guidePath), statePath
}

// guidePost 打引导页接口（用户实际操作路径）。
func guidePost(t *testing.T, base, path string, payload map[string]any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(payload)
	resp, err := http.Post(base+path, "application/json", strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("POST %s 失败：%v", path, err)
	}
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("POST %s 响应不是 JSON：%v", path, err)
	}
	return m
}

// guideGet 读引导页 /status。
func guideGet(t *testing.T, base, path string) map[string]any {
	t.Helper()
	resp, err := http.Get(base + path)
	if err != nil {
		t.Fatalf("GET %s 失败：%v", path, err)
	}
	defer resp.Body.Close()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("GET %s 响应不是 JSON：%v", path, err)
	}
	return m
}

// 缺陷 A：下发失败一次（讯飞侧频率限制）之后，同一条流程必须还能走通。
func TestSendFailureIsRecoverableInSameFlow(t *testing.T) {
	f := newFake(t, func(f *fakeUpstream) { f.sendFailLeft = 1 })
	base, statePath := startGuide(t)

	res := guidePost(t, base, apiSendCodePath, map[string]any{"phone": "13800138000"})
	if res["ok"] == true {
		t.Fatalf("首次下发应失败：%+v", res)
	}
	if msg, _ := res["message"].(string); !strings.Contains(msg, "频繁") {
		t.Errorf("失败原因应透传上游 desc，实际：%q", msg)
	}
	// 关键：不能就此终态。
	if _, err := Poll(NewClient(), statePath); err != ErrPending {
		t.Fatalf("下发失败后流程应仍 pending（可就地重试），实际 %v", err)
	}
	if st := CurrentStatus(); st.Phase != PhasePending || st.SendErr == "" {
		t.Fatalf("失败原因要可见但不得终态：%+v", st)
	}

	// 用户再点一次「获取验证码」→ 成功 → 直接提交，必须能入库。
	res2 := guidePost(t, base, apiSendCodePath, map[string]any{"phone": "13800138000"})
	if res2["ok"] != true {
		t.Fatalf("重试下发应成功：%+v", res2)
	}
	if st := CurrentStatus(); st.SendErr != "" {
		t.Errorf("一次成功下发应洗掉失败提示，实际残留 %q", st.SendErr)
	}
	ok := guidePost(t, base, apiLoginPath, map[string]any{
		"phone": "13800138000", "code": "123456", "msgid": testMsgID})
	if ok["ok"] != true {
		t.Fatalf("登录应成功（缺陷 A 的现场就是这里页面/面板说法相反）：%+v", ok)
	}
	r, err := Poll(NewClient(), statePath)
	if err != nil {
		t.Fatalf("Poll 必须取到结果，实际 %v", err)
	}
	if r.AccessToken != testSession || f.checkCount != 1 {
		t.Fatalf("结果/上游调用次数不符：res=%+v checkCount=%d", r, f.checkCount)
	}
}

// 缺陷 B：页面刷新后（msgid 不在请求里）提交仍应成功——服务端留了最近一次的 msgid。
func TestRefreshedPageRecoversMsgID(t *testing.T) {
	f := newFake(t, nil)
	base, statePath := startGuide(t)

	if res := guidePost(t, base, apiSendCodePath, map[string]any{"phone": "13800138000"}); res["ok"] != true {
		t.Fatalf("发验证码失败：%+v", res)
	}
	// 模拟刷新：payload 里没有 msgid（旧实现会直接回「请先点获取验证码」）。
	ok := guidePost(t, base, apiLoginPath, map[string]any{"phone": "13800138000", "code": "123456"})
	if ok["ok"] != true {
		t.Fatalf("刷新后提交应被服务端补回 msgid 并成功：%+v", ok)
	}
	f.mu.Lock()
	param, _ := f.lastBody["param"].(map[string]any)
	f.mu.Unlock()
	if got, _ := param["msgid"].(string); got != testMsgID {
		t.Fatalf("带回上游的 msgid 不符：%q want %q", got, testMsgID)
	}
	if _, err := Poll(NewClient(), statePath); err != nil {
		t.Fatalf("Poll 应取到结果：%v", err)
	}
}

// /status 是页面刷新恢复的通道：可以带 msgid，但绝不带凭据、绝不带手机号明文。
func TestStatusReturnsMsgIDWithoutCredentials(t *testing.T) {
	newFake(t, nil)
	base, _ := startGuide(t)
	if res := guidePost(t, base, apiSendCodePath, map[string]any{"phone": "13800138000"}); res["ok"] != true {
		t.Fatalf("发验证码失败：%+v", res)
	}
	raw, _ := json.Marshal(guideGet(t, base, apiStatusPath))
	if !strings.Contains(string(raw), testMsgID) {
		t.Fatalf("status 应回传 msgid 供刷新恢复：%s", raw)
	}
	for _, leak := range []string{testSession, "13800138000"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("status 泄露敏感值 %q：%s", leak, raw)
		}
	}
}

// 面板已收尾（Shutdown 把 cur 置 nil）后，残留标签页的提交必须被当场拒绝，
// 而不是「上游换到 session → 没人取走 → 页面却说登录成功」。
// 面板已收尾（Shutdown 把 cur 置 nil）后，残留标签页的提交必须被当场拒绝，
// 而不是「上游换到 session → 没人取走 → 页面却说登录成功」。
//
// 说明：Shutdown 会关掉 loopback 监听，所以真实的浏览器请求会直接连不上（JS 进 catch）；
// 但「监听还活着的竞态窗口」（已发请求与关闭并行、或以后改成复用监听）必须由
// isCurrent 守卫兜住，所以这里直接驱 handler 测守卫本身。
func TestLoginAfterShutdownIsRefused(t *testing.T) {
	f := newFake(t, nil)
	_, statePath := startGuide(t)
	s := currentSession()
	if s == nil {
		t.Fatal("Start 后应有一条进行中的会话")
	}
	if _, err := SendCode(NewClient(), "13800138000"); err != nil {
		t.Fatalf("发验证码失败：%v", err)
	}
	Shutdown() // 面板侧收尾（失败/取消都会走到这里）

	rec := httptest.NewRecorder()
	s.handleLogin(rec, httptest.NewRequest(http.MethodPost, apiLoginPath,
		strings.NewReader(`{"phone":"13800138000","code":"123456","msgid":"`+testMsgID+`"}`)))
	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("响应不是 JSON：%v（%s）", err, rec.Body)
	}
	if res["ok"] == true {
		t.Fatalf("流程已结束后不得回成功：%+v", res)
	}
	if msg, _ := res["message"].(string); !strings.Contains(msg, "已结束") {
		t.Errorf("应给出可行动提示（重新发起），实际：%q", msg)
	}
	if f.checkCount != 0 {
		t.Fatalf("拒绝必须发生在上游调用之前（不该白换一次 session），实际 checkCount=%d", f.checkCount)
	}
	if _, err := Poll(NewClient(), statePath); err != ErrPending {
		t.Fatalf("收尾后 Poll 应回 ErrPending，实际 %v", err)
	}
}

// 同一仓库里的另一个入口：直接调 `s.login`（不走引导页）时，已取消的会话不得被复活，
// 否则 Poll/落盘会拿到一个面板已放弃的凭据（与上面拒绝路径共用一个守卫）。
func TestLoginDoesNotReviveCancelledSession(t *testing.T) {
	newFake(t, nil)
	_, _ = startGuide(t)
	s := currentSession()
	if _, err := SendCode(NewClient(), "13800138000"); err != nil {
		t.Fatalf("发验证码失败：%v", err)
	}
	Shutdown()

	if _, err := s.login(NewClient(), "13800138000", "123456", testMsgID); err != nil {
		t.Fatalf("上游本身是成功的（不应在协议层报错）：%v", err)
	}
	if st := s.status(); st.Phase == PhaseSuccess || st.HasSession {
		t.Fatalf("已取消的会话不得被写回成功：%+v", st)
	}
}
