package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"wild-work/internal/config"
	"wild-work/internal/custom"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// appTempDir 替代 t.TempDir：自定义渠道会在该目录里起 pool（state.json / state-custom-*.json），
// Windows 下异步落盘与 Go 的自动清理会撞车，实测报
// 「TempDir RemoveAll cleanup: unlinkat …: The directory is not empty」或「文件被另一个进程占用」，
// 在全量并发跑时把不相干的测试带红。自己清理 = 带重试 + 最后仍有残留只记日志不判失败。
func appTempDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "ww-test-")
	if err != nil {
		t.Fatalf("建临时目录失败: %v", err)
	}
	t.Cleanup(func() {
		for i := 0; i < 60; i++ {
			if os.RemoveAll(d) == nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Logf("临时目录残留未清理（Windows 异步落盘，不影响断言）：%s", d)
	})
	return d
}

func TestCustomChannelsAPIAndHotReload(t *testing.T) {
	tmpDir := appTempDir(t)
	cfg := config.Default()
	cfg.StateFile = tmpDir + "/state.json"
	cfgPath := tmpDir + "/config.json"
	_ = config.Save(cfg, cfgPath)

	a, err := New(Options{
		ConfigPath: cfgPath,
		Config:     cfg,
		Runtimes:   map[provider.Kind]*Runtime{},
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer a.Close()

	var synced []custom.ChannelConfig
	a.SetCustomChannelsSyncer(func(channels []custom.ChannelConfig) {
		synced = channels
	})

	mux := http.NewServeMux()
	a.HandleAPI(mux)

	// 1. 测试 POST /api/config/custom_channel_test（假上游）
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"step-5-preview"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	testReq := map[string]any{
		"name":     "stepfun",
		"base_url": ts.URL,
		"api_key":  "test-key",
	}
	rawTest, _ := json.Marshal(testReq)
	req := httptest.NewRequest("POST", "/api/config/custom_channel_test", bytes.NewReader(rawTest))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("custom_channel_test status %d: %s", w.Code, w.Body.String())
	}
	var testResp struct {
		OK     bool     `json:"ok"`
		Models []string `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &testResp); err != nil || !testResp.OK {
		t.Fatalf("expected ok true with models, got: %s", w.Body.String())
	}
	if len(testResp.Models) != 1 || testResp.Models[0] != "step-5-preview" {
		t.Errorf("unexpected models: %v", testResp.Models)
	}

	// 2. 测试 POST /api/config/custom_channels 保存配置与触发回调
	saveReq := map[string]any{
		"channels": []map[string]any{
			{
				"name":     "stepfun",
				"base_url": ts.URL,
				"api_key":  "key1",
				"models":   []string{"step-5-preview"},
			},
		},
	}
	rawSave, _ := json.Marshal(saveReq)
	reqSave := httptest.NewRequest("POST", "/api/config/custom_channels", bytes.NewReader(rawSave))
	wSave := httptest.NewRecorder()
	mux.ServeHTTP(wSave, reqSave)
	if wSave.Code != 200 {
		t.Fatalf("custom_channels save status %d: %s", wSave.Code, wSave.Body.String())
	}
	if len(synced) != 1 || synced[0].Name != "stepfun" {
		t.Fatalf("expected custom channels syncer called with stepfun, got: %+v", synced)
	}

	// 3. 模拟 main.go 装配 custom runtimes 并测试 accountViews
	p := pool.New(tmpDir + "/state-custom.json")
	cClient := custom.New(synced[0])
	a.UpdateCustomRuntimes(map[provider.Kind]*Runtime{
		provider.Kind("stepfun"): {
			Kind:     provider.Kind("stepfun"),
			Pool:     p,
			Upstream: cClient,
			Custom:   true,
		},
	})

	if !a.creditsNAKind("stepfun") {
		t.Errorf("expected stepfun to be CreditsNA")
	}
	if !a.noExplicitCheckinKind(provider.Kind("stepfun")) {
		t.Errorf("expected stepfun to be noExplicitCheckinKind")
	}

	// 4. 测试 GET /api/state 包含 custom_channels（api_key 必须脱敏）
	st := a.GetState()
	if len(st.CustomChannels) != 1 || st.CustomChannels[0].Name != "stepfun" {
		t.Errorf("expected CustomChannels in state, got %+v", st.CustomChannels)
	}
	if st.CustomChannels[0].APIKey == "test-key" {
		t.Errorf("api_key 应以脱敏形式回显，不得明文送进面板载荷：%q", st.CustomChannels[0].APIKey)
	}
}

// TestCustomChannelKeyMaskedRoundTrip 验证 api_key 的「脱敏回显 → 原样保存 → 真实密钥不丢」链路。
// 面板回显的是脱敏值（含 …），用户没改动该渠道时提交回来的仍是脱敏串；
// 若原样落盘就把真实密钥换成了废值，下次请求全部 401。
func TestCustomChannelKeyMaskedRoundTrip(t *testing.T) {
	tmpDir := appTempDir(t)
	cfg := config.Default()
	cfg.StateFile = tmpDir + "/state.json"
	cfgPath := tmpDir + "/config.json"
	_ = config.Save(cfg, cfgPath)
	a, err := New(Options{ConfigPath: cfgPath, Config: cfg, Runtimes: map[provider.Kind]*Runtime{}})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer a.Close()

	if err := a.SetCustomChannels([]custom.ChannelConfig{
		{Name: "stepfun", BaseURL: "https://api.stepfun.com/v1", APIKey: "sk-real-secret-1234"},
	}); err != nil {
		t.Fatalf("SetCustomChannels: %v", err)
	}

	// 模拟面板：取脱敏回显值，不改动直接回传（真实前端的常见路径）
	shown := a.GetState().CustomChannels
	if shown[0].APIKey == "sk-real-secret-1234" || !isMaskedKey(shown[0].APIKey) {
		t.Fatalf("回显应为脱敏形态：%q", shown[0].APIKey)
	}
	if err := a.SetCustomChannels(shown); err != nil {
		t.Fatalf("回存脱敏值应成功（内部找回真实密钥）: %v", err)
	}
	// 落盘与内存里的真实密钥必须原样保留
	if got := a.runtime(provider.Kind("stepfun")); got != nil {
		// runtime 由 syncer 装配，此测试未注入 syncer，跳过
		_ = got
	}
	saved, _ := config.Load(cfgPath)
	if len(saved.CustomChannels) != 1 || saved.CustomChannels[0].APIKey != "sk-real-secret-1234" {
		t.Fatalf("真实密钥被脱敏串覆盖了：%+v", saved.CustomChannels)
	}

	// 反向：显式传新密钥应当生效
	if err := a.SetCustomChannels([]custom.ChannelConfig{
		{Name: "stepfun", BaseURL: "https://api.stepfun.com/v1", APIKey: "sk-new-key-9999"},
	}); err != nil {
		t.Fatalf("SetCustomChannels(new key): %v", err)
	}
	saved2, _ := config.Load(cfgPath)
	if saved2.CustomChannels[0].APIKey != "sk-new-key-9999" {
		t.Fatalf("显式新密钥未生效：%+v", saved2.CustomChannels)
	}
}

// TestFeesOrderIncludesDynamicChannels 验证费率表会展示动态（自定义）渠道。
// feeChannelOrder 是静态表，若不把 modelsByKind 里的未登记渠道追加进去，
// 面板费率表永远不出该渠道 tab（R66）。
func TestFeesOrderIncludesDynamicChannels(t *testing.T) {
	models := map[provider.Kind][]provider.ModelInfo{
		provider.WorkBuddy:       {{ID: "glm-5.2"}},
		provider.Kind("stepfun"): {{ID: "step-5-preview"}},
		provider.Kind("acustom"): {{ID: "x"}}, // 验证排序稳定
	}
	order := feeChannelOrderWithDynamic(models)
	last := order[len(order)-3:]
	want := []provider.Kind{provider.Kind("acustom"), provider.Kind("stepfun")}
	// 静态表末尾之後应接按名排序的动态渠道
	if len(order) != len(feeChannelOrder)+2 {
		t.Fatalf("want %d channels, got %d: %v", len(feeChannelOrder)+2, len(order), order)
	}
	if last[1] != want[0] || last[2] != want[1] {
		t.Errorf("动态渠道应追加到末尾且按名排序，got tail %v", last)
	}
	// 无动态渠道时原样返回静态表
	if got := feeChannelOrderWithDynamic(map[provider.Kind][]provider.ModelInfo{provider.WorkBuddy: {{ID: "x"}}}); len(got) != len(feeChannelOrder) {
		t.Errorf("无动态渠道时应返回静态表，got %d", len(got))
	}
}

// TestRuntimeTableConcurrentAccess 并发安全：保存自定义渠道（写 runtimes 表）与
// 后台统计喂入/面板取数（读 runtimes 表）并发时不得触发
// `fatal error: concurrent map read and map write`（不可 recover，daemon 直接死）。
// 原始实现里 UpdateCustomRuntimes 在 a.mu 下写 map，而 allStatuses/runtime 等
// 9 处读点全程不加锁 —— 这是本仓首个 post-init 写（此前 runtimes 只在 init 赋值）。
func TestRuntimeTableConcurrentAccess(t *testing.T) {
	tmpDir := appTempDir(t)
	cfg := config.Default()
	cfg.StateFile = tmpDir + "/state.json"
	cfgPath := tmpDir + "/config.json"
	_ = config.Save(cfg, cfgPath)
	a, err := New(Options{ConfigPath: cfgPath, Config: cfg, Runtimes: map[provider.Kind]*Runtime{}})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer a.Close()

	mk := func(name string) map[provider.Kind]*Runtime {
		return map[provider.Kind]*Runtime{
			provider.Kind(name): {
				Kind:   provider.Kind(name),
				Pool:   pool.New(tmpDir + "/state-" + name + ".json"),
				Custom: true,
			},
		}
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// 写：反复替换自定义渠道集合（模拟面板反复保存）
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			a.UpdateCustomRuntimes(mk([]string{"a", "b", "c"}[i%3]))
		}
	}()
	// 读：并发遍历运行时表（模拟 30s 统计喂入器与 /api/state）
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = a.allStatuses()
				_ = a.runtimesSnapshot()
				_ = a.runtime(provider.Kind("a"))
				_ = a.creditsNAKind("a")
			}
		}()
	}
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestCustomChannelTestEndpointResolvesMaskedKey 面板「测试连通」必须能在 api_key 为
// 脱敏回显态时用真实密钥（否则未改动过的渠道测试恒 401，同 #117 的探针不同源问题）。
// 端点在收到含 「…」 的 key 时按 name 从配置找回真实密钥。
func TestCustomChannelTestEndpointResolvesMaskedKey(t *testing.T) {
	tmpDir := appTempDir(t)
	cfg := config.Default()
	cfg.StateFile = tmpDir + "/state.json"
	cfgPath := tmpDir + "/config.json"
	_ = config.Save(cfg, cfgPath)
	a, err := New(Options{ConfigPath: cfgPath, Config: cfg, Runtimes: map[provider.Kind]*Runtime{}})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer a.Close()

	// 假上游：记录收到的 Authorization 头，据此断言端点用的是真实密钥
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
	}))
	defer ts.Close()

	if err := a.SetCustomChannels([]custom.ChannelConfig{
		{Name: "stepfun", BaseURL: ts.URL, APIKey: "sk-real-secret-abcdefgh"},
	}); err != nil {
		t.Fatalf("SetCustomChannels: %v", err)
	}

	mux := http.NewServeMux()
	a.HandleAPI(mux)
	body, _ := json.Marshal(map[string]any{
		"name":     "stepfun",
		"base_url": ts.URL,
		"api_key":  maskKey("sk-real-secret-abcdefgh"), // 脱敏回显态
	})
	req := httptest.NewRequest("POST", "/api/config/custom_channel_test", bytes.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if gotAuth != "Bearer sk-real-secret-abcdefgh" {
		t.Fatalf("端点应用真实密钥而非脱敏值，got %q", gotAuth)
	}
}

// TestCustomChannelTestEndpointReturnsCaps 「导入/测试」端点除纯 ID 外还要回能力清单（R119）：
// 前端勾选导入时靠 caps 写配置，拿不到的字段必须留空（而不是让前端补 1M/64k 那种猜测值）。
// models 字段保持旧形状（[]string），不破既有契约。
func TestCustomChannelTestEndpointReturnsCaps(t *testing.T) {
	tmpDir := appTempDir(t)
	cfg := config.Default()
	cfg.StateFile = tmpDir + "/state.json"
	cfgPath := tmpDir + "/config.json"
	_ = config.Save(cfg, cfgPath)
	a, err := New(Options{ConfigPath: cfgPath, Config: cfg, Runtimes: map[provider.Kind]*Runtime{}})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer a.Close()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
		  {"id":"with-cap","context_window":1048576,"max_tokens":65536,"supported_parameters":["tools","reasoning"]},
		  {"id":"bare","object":"model","owned_by":"someone"}]}`))
	}))
	defer ts.Close()

	mux := http.NewServeMux()
	a.HandleAPI(mux)
	body, _ := json.Marshal(map[string]any{"name": "t1", "base_url": ts.URL, "api_key": "sk-x"})
	req := httptest.NewRequest("POST", "/api/config/custom_channel_test", bytes.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		OK     bool              `json:"ok"`
		Models []string          `json:"models"`
		Caps   []custom.ModelCap `json:"caps"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应形状变了（models 必须仍是 []string）: %v — %s", err, w.Body.String())
	}
	if !resp.OK || len(resp.Models) != 2 || resp.Models[0] != "with-cap" {
		t.Fatalf("models 回退形状不符: %s", w.Body.String())
	}
	if len(resp.Caps) != 2 {
		t.Fatalf("caps 数 = %d, want 2", len(resp.Caps))
	}
	byID := map[string]custom.ModelCap{}
	for _, c := range resp.Caps {
		byID[c.ID] = c
	}
	got := byID["with-cap"]
	if got.ContextWindow != 1048576 || got.MaxTokens != 65536 || got.CapSource != custom.CapSourceAPI {
		t.Fatalf("能力字段没解析出来：%+v", got)
	}
	if got.SupportsTools == nil || !*got.SupportsTools {
		t.Fatalf("supported_parameters 里的 tools 应命中：%+v", got)
	}
	bare := byID["bare"]
	if bare.CapSource != "" || bare.ContextWindow != 0 {
		t.Fatalf("无能力字段的上游被填了猜测值：%+v", bare)
	}
}

// TestCustomChannelKeyRevealEndpoint 点「复制 key」才下发的那条接口（R119）：
// ① 明文只在被点名时回一次；② 未知渠道报错；③ 渠道列表/费率载荷里绝不得出现明文。
func TestCustomChannelKeyRevealEndpoint(t *testing.T) {
	tmpDir := appTempDir(t)
	cfg := config.Default()
	cfg.StateFile = tmpDir + "/state.json"
	cfgPath := tmpDir + "/config.json"
	_ = config.Save(cfg, cfgPath)
	a, err := New(Options{ConfigPath: cfgPath, Config: cfg, Runtimes: map[provider.Kind]*Runtime{}})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer a.Close()

	const realKey = "sk-reveal-secret-0123456789"
	if err := a.SetCustomChannels([]custom.ChannelConfig{
		{Name: "stepfun", BaseURL: "https://api.stepfun.com/v1", APIKey: realKey},
	}); err != nil {
		t.Fatalf("SetCustomChannels: %v", err)
	}
	mux := http.NewServeMux()
	a.HandleAPI(mux)

	post := func(path, body string) (int, string) {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}

	code, buf := post("/api/config/custom_channel_key", `{"name":"stepfun"}`)
	if code != 200 || !strings.Contains(buf, realKey) {
		t.Fatalf("按需取明文失败：%d %s", code, buf)
	}
	// 渠道名大小写不敏感（resolveCustomKey 口径）
	if code, _ := post("/api/config/custom_channel_key", `{"name":"STEPFUN"}`); code != 200 {
		t.Fatalf("渠道名应大小写不敏感，got %d", code)
	}
	if code, buf := post("/api/config/custom_channel_key", `{"name":"nobody"}`); code != 404 || strings.Contains(buf, realKey) {
		t.Fatalf("未知渠道应 404 且不泄漏任何 key：%d %s", code, buf)
	}

	// 反面：列表类接口只给脱敏值
	st, _ := json.Marshal(a.GetState())
	if strings.Contains(string(st), realKey) {
		t.Fatalf("/api/state 里出现明文 key")
	}
	feesBody, _ := json.Marshal(a.FeesInfo())
	if strings.Contains(string(feesBody), realKey) {
		t.Fatalf("/api/fees 里出现明文 key")
	}
}

// TestFeesCarriesCustomAccessAndCapSource 费率页要能显示接入点 + 脱敏 key + 能力来源（R119）。
// 故意走 FeesInfo() 全链路而不是单测 applyCustomFeesMeta：漏接调用点（只写了方法没调）
// 必须当场变红，否则面板永远看不到接入点。明文不得出现、脱敏串必须出现，也在同一条里证。
func TestFeesCarriesCustomAccessAndCapSource(t *testing.T) {
	tmpDir := appTempDir(t)
	cfg := config.Default()
	cfg.StateFile = tmpDir + "/state.json"
	cfgPath := tmpDir + "/config.json"
	_ = config.Save(cfg, cfgPath)
	a, err := New(Options{ConfigPath: cfgPath, Config: cfg, Runtimes: map[provider.Kind]*Runtime{}})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer a.Close()

	const realKey = "sk-fees-secret-abcdefgh"
	cc := custom.ChannelConfig{
		Name: "stepfun", BaseURL: "https://api.stepfun.com/v1", APIKey: realKey,
		Models: []custom.ModelConfig{
			{ID: "step-5-preview", ContextWindow: 1048576, CapSource: custom.CapSourceAPI},
			{ID: "handy", ContextWindow: 64000, CapSource: custom.CapSourceUser},
			{ID: "dark"},
		},
	}
	if err := a.SetCustomChannels([]custom.ChannelConfig{cc}); err != nil {
		t.Fatalf("SetCustomChannels: %v", err)
	}
	// 模拟 main.go 装配：staticModelFallback 只认 Custom 渠道，不装配则费率页没有这一组
	a.UpdateCustomRuntimes(map[provider.Kind]*Runtime{
		provider.Kind("stepfun"): {Kind: provider.Kind("stepfun"), Pool: pool.New(tmpDir + "/state-custom.json"), Upstream: custom.New(cc), Custom: true},
	})

	raw, err := json.Marshal(a.FeesInfo())
	if err != nil {
		t.Fatalf("marshal FeesInfo: %v", err)
	}
	if strings.Contains(string(raw), realKey) {
		t.Fatalf("费率载荷里出现明文 key：%s", raw)
	}
	var got struct {
		Channels []feesChannel `json:"channels"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var wb, sf *feesChannel
	for i := range got.Channels {
		switch got.Channels[i].Channel {
		case "stepfun":
			sf = &got.Channels[i]
		case "workbuddy":
			wb = &got.Channels[i]
		}
	}
	if sf == nil {
		t.Fatalf("费率表里没有自定义渠道分组（staticModelFallback 未生效）：%s", raw)
	}
	if sf.BaseURL != "https://api.stepfun.com/v1" {
		t.Fatalf("base_url = %q", sf.BaseURL)
	}
	if sf.APIKeyMasked == "" || !isMaskedKey(sf.APIKeyMasked) {
		t.Fatalf("key 必须是脱敏串，got %q", sf.APIKeyMasked)
	}
	// 内建渠道不填（endpoint 写死在代码里，展示了也没意义）
	if wb != nil && (wb.BaseURL != "" || wb.APIKeyMasked != "") {
		t.Fatalf("内建渠道被填了接入点：%+v", *wb)
	}
	wantSrc := map[string]string{"step-5-preview": "api", "handy": "user", "dark": ""}
	if len(sf.Models) != len(wantSrc) {
		t.Fatalf("模型行数 = %d, want %d", len(sf.Models), len(wantSrc))
	}
	for _, m := range sf.Models {
		want, ok := wantSrc[m.Model]
		if !ok {
			t.Fatalf("多出模型 %q", m.Model)
		}
		if m.CapSource != want {
			t.Fatalf("%s 的来源标注 = %q, want %q", m.Model, m.CapSource, want)
		}
		// 无来源的那行不得把上下文当已知展示（前端 ctxTag 判 has_context）；
		// 有来源的必须置 has_context，否则就是 bug1（自定义渠道上下文恒不显示）复发。
		if want == "" && m.HasContext {
			t.Fatalf("无来源的上下文被标成可展示：%+v", m)
		}
		if want != "" && !m.HasContext {
			t.Fatalf("有来源的上下文却没置 has_context（bug1 复发）：%+v", m)
		}
	}
}

// TestMaskKeyKeepsLongPrefix 脱敏必须留住长前缀（R121）：第三方 key 的辨识度全在前缀上
// （千帆 bce-v3/ALTAK…、OpenAI sk-proj-…），旧口径只留 5 个字符，等于让人在一堆
// 「sk-12…cdef」里猜哪家是哪家。同时不得假定密钥形如 sk-，也不得泄漏原文长度。
func TestMaskKeyKeepsLongPrefix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		// 短到露头+尾就等于露大半原文 ⇒ 全遮
		{"sk-12345678901", "****"},
		// 千帆形态（30 字符）：头部收到 11 个，足以认出 bce-v3/ALTAK
		{"bce-v3/ALTAK-1a2b3c4d5e6f7g8h", "bce-v3/ALT****7g8h"},
		{"sk-proj-AbCdEfGhIjKlMnOpQrSt", "sk-proj-Ab****QrSt"}, // 前缀 sk-proj 完整可见
		{"abcdefghijklmnopqrst", "abcdef****qrst"},             // 20 字符只允许露一半
		{"密钥密钥密钥密钥密钥密钥密钥密钥密钥密钥", "密钥密钥密钥****密钥密钥"},             // 按字符不按字节切
		{"   spaced-key-example-12   ", "spaced****e-12"},      // 先 trim 再算
	}
	for _, c := range cases {
		if got := maskKey(c.in); got != c.want {
			t.Errorf("maskKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// 结构性约束：标记永远是四星（不随原文长度变化，否则等于泄露长度），
	// 且可见字符总量不超原文一半。
	for _, k := range []string{"sk-a", "sk-aaaaaaaaaaaa1111", "sk-aaaaaaaaaaaa1111222233334444", "bce-v3/ALTAK-1a2b3c4d5e6f7g8h9i0j"} {
		m := maskKey(k)
		if m == maskedMark {
			continue // 太短直接全遮，本来就不该露任何原文
		}
		if strings.Count(m, "*") != 4 || !strings.Contains(m, maskedMark) {
			t.Errorf("maskKey(%q) = %q：标记形态不对", k, m)
			continue
		}
		head := strings.Index(m, maskedMark)
		if vis := head + 4; vis > len([]rune(k))/2 {
			t.Errorf("maskKey(%q) = %q：露出 %d 字符超原文一半", k, m, vis)
		}
		if head < 4 && len([]rune(k)) >= 20 {
			t.Errorf("maskKey(%q) = %q：前缀太短认不出是哪家", k, m)
		}
	}
}

// TestIsMaskedKeyRecognizesLegacyMarks 脱敏串同时是「面板未改动此字段」的记号，
// 改格式时必须新旧都认：升级瞬间浏览器里可能还渲染着旧脱敏串，当成真值落盘
// 会把可用密钥洗成废值（前端 app.js 有同名判据，两侧必须同步）。
func TestIsMaskedKeyRecognizesLegacyMarks(t *testing.T) {
	for _, s := range []string{"sk-real****cdef", "sk-rea…cdef", "sk-…", "••••••", "bce-v3/ALTAK****7g8h"} {
		if !isMaskedKey(s) {
			t.Errorf("%q 应判为脱敏串", s)
		}
	}
	for _, s := range []string{"sk-real-secret-abcdefgh", "bce-v3/ALTAK-1a2b3c4d", "x"} {
		if isMaskedKey(s) {
			t.Errorf("%q 是真密钥，不得判为脱敏串", s)
		}
	}
}

// TestCustomChannelTestEndpointLocatesBase 「测试/导入」端点必须先定位真实根再探测（R121）：
// 用户粘 http://host 而网关实际挂在 /v1 下时，回体要给出纠正后的根，前端才能回填。
// 定位失败时也要 200 + 逐条 tries，让面板说得出「试了哪几个地址、各回什么」，
// 而不是只有一句「连接失败」；明文密钥绝不得出现在响应里。
func TestCustomChannelTestEndpointLocatesBase(t *testing.T) {
	tmpDir := appTempDir(t)
	cfg := config.Default()
	cfg.StateFile = tmpDir + "/state.json"
	cfgPath := tmpDir + "/config.json"
	_ = config.Save(cfg, cfgPath)
	a, err := New(Options{ConfigPath: cfgPath, Config: cfg, Runtimes: map[provider.Kind]*Runtime{}})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer a.Close()

	// 网关只认 {root}/v1/models；{root}/models 与一切别的路径都 404
	var seen []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"glm-4","context_window":131072}]}`))
			return
		}
		if r.URL.Path == "/v1/chat/completions" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	post := func(payload map[string]any) map[string]any {
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest("POST", "/api/config/custom_channel_test", bytes.NewReader(body))
		w := httptest.NewRecorder()
		mux := http.NewServeMux()
		a.HandleAPI(mux)
		mux.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("响应不是 JSON：%s", w.Body.String())
		}
		return out
	}

	got := post(map[string]any{"name": "glm", "base_url": ts.URL, "api_key": "sk-secret-abcdefghij"})
	if got["ok"] != true {
		t.Fatalf("应定位成功：%v", got)
	}
	if got["base_url"] != ts.URL+"/v1" || got["adjusted"] != true {
		t.Fatalf("应把根补成 %s/v1 并标注 adjusted：%v", ts.URL, got)
	}
	if got["models_ok"] != true || got["chat_ok"] != true {
		t.Fatalf("目录与对话端点都应确认：%v", got)
	}
	if ids, _ := got["models"].([]any); len(ids) != 1 || ids[0] != "glm-4" {
		t.Fatalf("models 形状变了：%v", got["models"])
	}
	if tries, _ := got["tries"].([]any); len(tries) == 0 {
		t.Fatalf("探测明细必须留痕：%v", got)
	}
	if js, _ := json.Marshal(got); strings.Contains(string(js), "sk-secret-abcdefghij") {
		t.Fatalf("响应里泄漏了明文密钥：%v", got)
	}

	// 全线 404：必须 ok=false 且给出错误与逐条 tries（Fail Early，不带着死配置继续跑）
	dead := post(map[string]any{"name": "dead", "base_url": "http://127.0.0.1:1/v1", "api_key": "k"})
	if dead["ok"] == true || dead["error"] == nil || dead["tries"] == nil {
		t.Fatalf("定位失败要如实回传：%v", dead)
	}
	for _, p := range seen {
		if strings.HasSuffix(p, "/chat/completions") && !strings.HasPrefix(p, "/v1/") {
			t.Fatalf("去探测了猜测的根：%s", p)
		}
	}
}
