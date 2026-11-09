// 降级链别名（R129）在兼容层三条路径上的行为锁定：
//   - 别名 > compat.model_map（精确 → 通配）> compat.default_channel
//   - /v1/responses、/v1/messages、/v1/messages/count_tokens 三处一致
//   - 别名表热更新后立即生效（不必重启）
//   - 未知别名与未知模型错误同构（400 + invalid_model / invalid_request_error）
package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// aliasRouter 带别名表的路由规则：fast → workbuddy/glm-5.2，且 gpt-5 同时被
// model_map 映射到 qoder/gmodel、被 default_channel 兜底到 workbuddy/gpt-5。
// 三处重合正是「优先级」必须被锁死的场景。
func aliasRouter() Router {
	return Router{
		Default:  "workbuddy",
		Map:      map[string]string{"gpt-5": "qoder/gmodel", "claude-*": "qoder/gmodel"},
		Channels: []string{"workbuddy", "qoder"},
		Aliases:  map[string]string{"fast": "workbuddy/glm-5.2", "gpt-5": "workbuddy/alias-wins"},
	}
}

// newAliasGateway 构造兼容层 + 外层 mux（与 main 装配方式一致）。
func newAliasGateway(inner http.Handler) (*Gateway, *http.ServeMux) {
	g := New(Config{Inner: inner, APIKey: "secret", Router: aliasRouter()})
	mux := http.NewServeMux()
	g.Routes(mux)
	mux.Handle("/", inner)
	return g, mux
}

// lastModel 取内层收到的最后一次请求体里的 model（内层实际被要求执行的模型）。
func lastModel(t *testing.T, h *scriptedHandler) string {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.calls) == 0 {
		t.Fatal("内层未收到任何请求")
	}
	parts := strings.Split(h.calls[len(h.calls)-1], "|")
	return parts[len(parts)-1]
}

// TestAliasOnResponses Responses 路径：别名解析成主力模型全路径，内层收到的是它；
// 响应体 model 仍回填客户端请求名（= 别名）。
func TestAliasOnResponses(t *testing.T) {
	inner := &scriptedHandler{}
	_, mux := newAliasGateway(inner)
	rec := post(mux, "/v1/responses", `{"model":"fast","input":"hi"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := lastModel(t, inner); got != "workbuddy/glm-5.2" {
		t.Errorf("内层收到的 model = %q，want 别名解析后的 workbuddy/glm-5.2", got)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["model"] != "fast" {
		t.Errorf("响应体 model = %v，want 客户端请求名 fast", resp["model"])
	}
}

// TestAliasOnAnthropicMessages Anthropic 路径：同上。
func TestAliasOnAnthropicMessages(t *testing.T) {
	inner := &scriptedHandler{}
	_, mux := newAliasGateway(inner)
	rec := post(mux, "/v1/messages",
		`{"model":"fast","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := lastModel(t, inner); got != "workbuddy/glm-5.2" {
		t.Errorf("内层收到的 model = %q，want workbuddy/glm-5.2", got)
	}
}

// TestAliasOnCountTokens count_tokens 不发上游，但也必须认别名（否则三个接口口径不一致）；
// 未知名字必须与另两个接口同构地报错（同样需要去掉 default_channel 才能造出「未知」）。
func TestAliasOnCountTokens(t *testing.T) {
	_, mux := newAliasGateway(&scriptedHandler{})
	ok := post(mux, "/v1/messages/count_tokens",
		`{"model":"fast","messages":[{"role":"user","content":"你好，世界"}]}`)
	if ok.Code != http.StatusOK {
		t.Fatalf("别名应被 count_tokens 接受，实际 %d: %s", ok.Code, ok.Body.String())
	}

	rt := aliasRouter()
	rt.Default = ""
	g := New(Config{Inner: &scriptedHandler{}, APIKey: "secret", Router: rt})
	mux2 := http.NewServeMux()
	g.Routes(mux2)
	mux2.Handle("/", &scriptedHandler{})
	bad := post(mux2, "/v1/messages/count_tokens",
		`{"model":"nosuchalias","messages":[{"role":"user","content":"hi"}]}`)
	if bad.Code != http.StatusBadRequest || !strings.Contains(bad.Body.String(), "invalid_request_error") {
		t.Fatalf("未知名字应 400 invalid_request_error，实际 %d: %s", bad.Code, bad.Body.String())
	}
	if !strings.Contains(bad.Body.String(), "alias") {
		t.Errorf("错误文案应提示去 /v1/models 找带 alias 标记的条目，实际 %s", bad.Body.String())
	}
}

// TestAliasBeatsModelMapAndDefault 优先级：别名 > model_map（精确 → 通配）> default_channel。
// 把优先级改成 model_map 优先，本用例立刻变红。
func TestAliasBeatsModelMapAndDefault(t *testing.T) {
	rt := aliasRouter()
	cases := []struct{ in, want string }{
		{"fast", "workbuddy/glm-5.2"},                  // 仅别名命中
		{"gpt-5", "workbuddy/alias-wins"},              // 别名与 model_map 精确键重名 ⇒ 别名赢
		{"claude-sonnet-4-5", "qoder/gmodel"},          // 仅通配 map 命中
		{"something-else", "workbuddy/something-else"}, // 仅 default_channel 命中
		{"workbuddy/glm-5.2", "workbuddy/glm-5.2"},     // 已带前缀：行为完全不变
	}
	for _, c := range cases {
		got, err := rt.Resolve(c.in)
		if err != nil || got != c.want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

// TestAliasHotReload 别名表热更新后立即生效（面板保存降级链 ⇒ SetAliases）。
func TestAliasHotReload(t *testing.T) {
	inner := &scriptedHandler{}
	g, mux := newAliasGateway(inner)
	// 启动态无别名 ⇒ 走 default_channel
	if rec := post(mux, "/v1/responses", `{"model":"cheap","input":"hi"}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := lastModel(t, inner); got != "workbuddy/cheap" {
		t.Fatalf("无别名时应走 default_channel，实际 %q", got)
	}
	g.SetAliases(map[string]string{"cheap": "qoder/gmodel"})
	if rec := post(mux, "/v1/responses", `{"model":"cheap","input":"hi"}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := lastModel(t, inner); got != "qoder/gmodel" {
		t.Fatalf("热更新后应立即按别名路由，实际 %q", got)
	}
	// 反向：清空别名表 ⇒ 回到 default_channel（删规则即别名消失）
	g.SetAliases(nil)
	if rec := post(mux, "/v1/responses", `{"model":"cheap","input":"hi"}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := lastModel(t, inner); got != "workbuddy/cheap" {
		t.Fatalf("清空别名后应回落到 default_channel，实际 %q", got)
	}
}

// TestSetCompatKeepsAliases 保存 compat（SetCompat）不得把别名表刷回空——
// 两条热更新通道互不覆盖，否则「先存别名、后存 model_map」会让别名静默失效。
func TestSetCompatKeepsAliases(t *testing.T) {
	inner := &scriptedHandler{}
	g, mux := newAliasGateway(inner)
	g.SetAliases(map[string]string{"fast": "workbuddy/glm-5.2"})
	g.SetCompat("workbuddy", 0, map[string]string{"gpt-5": "qoder/gmodel"}, []string{"workbuddy", "qoder"})
	if rec := post(mux, "/v1/responses", `{"model":"fast","input":"hi"}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := lastModel(t, inner); got != "workbuddy/glm-5.2" {
		t.Fatalf("SetCompat 不得清掉别名表，实际路由到 %q", got)
	}
}

// TestUnknownAliasErrorShape 未知别名与未知模型错误同构：Responses 侧 400 + invalid_model。
// 这里必须去掉 default_channel——配了兜底渠道时任何裸名都会被打上渠道前缀（既有语义），
// 根本不存在「未知名字」这回事。
func TestUnknownAliasErrorShape(t *testing.T) {
	rt := aliasRouter()
	rt.Default = ""
	g := New(Config{Inner: &scriptedHandler{}, APIKey: "secret", Router: rt})
	mux := http.NewServeMux()
	g.Routes(mux)
	mux.Handle("/", &scriptedHandler{})

	rec := post(mux, "/v1/responses", `{"model":"nosuchalias","input":"hi"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d，want 400（与未知模型同构）", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"code":"invalid_model"`) {
		t.Errorf("错误码应为 invalid_model，实际 %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "alias") {
		t.Errorf("错误文案应提示可用别名位置，实际 %s", rec.Body.String())
	}

	// Anthropic 侧同构：400 + invalid_request_error（协议形状不同，状态码一致）
	recA := post(mux, "/v1/messages", `{"model":"nosuchalias","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	if recA.Code != http.StatusBadRequest || !strings.Contains(recA.Body.String(), "invalid_request_error") {
		t.Errorf("Anthropic 侧应 400 invalid_request_error，实际 %d: %s", recA.Code, recA.Body.String())
	}
}

// TestNoAliasConfiguredNoNoise 未配置别名时错误文案不得出现 alias 噪声
// （否则老用户会被一句无关的提示干扰）。
func TestNoAliasConfiguredNoNoise(t *testing.T) {
	rt := Router{Default: "workbuddy", Channels: []string{"workbuddy"}}
	if _, err := rt.Resolve("x/y"); err == nil {
		t.Fatal("未知渠道应报错")
	}
	if _, err := rt.Resolve(""); err == nil {
		t.Fatal("空名应报错")
	}
	rt2 := Router{Channels: []string{"workbuddy"}}
	_, err := rt2.Resolve("bare")
	if err == nil {
		t.Fatal("无 default_channel 的裸名应报错")
	}
	if strings.Contains(err.Error(), "alias") {
		t.Errorf("未配置别名时不得出现 alias 提示：%v", err)
	}
}

// aliasOnlyInner 只用于确认 count_tokens 不触发内层调用（它纯本地估算）。
func TestCountTokensDoesNotCallInner(t *testing.T) {
	inner := &scriptedHandler{}
	_, mux := newAliasGateway(inner)
	rec := post(mux, "/v1/messages/count_tokens",
		`{"model":"fast","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	inner.mu.Lock()
	n := len(inner.calls)
	inner.mu.Unlock()
	if n != 0 {
		t.Errorf("count_tokens 不得调用内层，实际 %d 次", n)
	}
}

// 确保测试用到的辅助类型与 httptest 仍被引用（避免未来删用例时留下未用导入）。
var _ = httptest.NewRecorder
