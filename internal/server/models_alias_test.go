// /v1/models 的别名条目（R129）行为锁定：
//   - id = 别名，owned_by = 主力模型所在渠道
//   - 能力字段逐项继承主力模型（不新增任何推断）
//   - 多一个 alias 字段指向主力全路径
//   - 别名条目排在真实模型之前
//   - 主力渠道未接入账号时别名不出现
//   - 别名表热更新后立即生效（每次请求现拼，无缓存失效通道）
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/config"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// modelsUpstream 静态模型表的假上游（不触发任何网络请求）。
type modelsUpstream struct {
	infos []provider.ModelInfo
}

func (u *modelsUpstream) RefreshToken(*auth.Auth) error                        { return nil }
func (u *modelsUpstream) FetchModels(*auth.Auth) ([]provider.ModelInfo, error) { return u.infos, nil }
func (u *modelsUpstream) FetchModelPricing(*auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}
func (u *modelsUpstream) UserResource(*auth.Auth) (int64, error) { return 0, nil }
func (u *modelsUpstream) UserResourceDetail(*auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}
func (u *modelsUpstream) DailyCheckin(*auth.Auth) error         { return nil }
func (u *modelsUpstream) Classify(int, string) provider.ErrKind { return provider.ErrServer }
func (u *modelsUpstream) ChatStream(*auth.Auth, []byte) (io.ReadCloser, int, []byte, error) {
	return nil, 0, nil, nil
}
func (u *modelsUpstream) Stream(http.ResponseWriter, io.Reader, string) (map[string]any, error) {
	return nil, nil
}
func (u *modelsUpstream) Aggregate(io.Reader, string) (map[string]any, error) { return nil, nil }

// richModel 带齐所有能力维度，供「逐字段继承」断言（含 nil 的 CanDisableThinking 反例另设）。
func richModel() provider.ModelInfo {
	yes := true
	return provider.ModelInfo{
		ID: "deepseek-v4.1-flash", Name: "DeepSeek V4.1 Flash",
		ContextWindow: 200000, MaxTokens: 8192, SupportsTools: true,
		SupportsImages: true, ContextFromAPI: true,
		ReasoningEfforts: []string{"low", "high"}, DefaultReasoningEffort: "high",
		CanDisableThinking: &yes,
	}
}

// newModelsHandler 装配一个渠道（workbuddy）+ 静态模型表 + 给定降级链。
func newModelsHandler(t *testing.T, rules []config.ModelFallbackRule, infos []provider.ModelInfo, withAccount bool) *Handler {
	t.Helper()
	p := pool.New(t.TempDir() + "/state-m.json")
	if withAccount {
		p.Add(&auth.Auth{UID: "m-uid"})
	}
	return NewHandler(Config{
		Runtimes: map[provider.Kind]*Runtime{
			provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: p, Upstream: &modelsUpstream{infos: infos}, StaticModels: infos},
		},
		APIKey: "k", ModelFallbacks: rules,
	})
}

// getModels 发一次 GET /v1/models，返回解析后的 data 数组。
func getModels(t *testing.T, h *Handler) []map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v (%s)", err, rec.Body.String())
	}
	return resp.Data
}

// TestModelsAliasEntryInheritsCapabilities 别名条目的能力字段与主力模型**逐字段一致**，
// 且只多一个 alias 字段（不编造任何上游没声明的能力）。
func TestModelsAliasEntryInheritsCapabilities(t *testing.T) {
	rules := []config.ModelFallbackRule{
		{From: "workbuddy/deepseek-v4.1-flash", To: []string{"workbuddy/other"}, Name: "fast"},
	}
	h := newModelsHandler(t, rules, []provider.ModelInfo{richModel()}, true)
	data := getModels(t, h)

	var alias, real map[string]any
	for _, e := range data {
		if e["id"] == "fast" {
			alias = e
		}
		if e["id"] == "workbuddy/deepseek-v4.1-flash" {
			real = e
		}
	}
	if alias == nil || real == nil {
		t.Fatalf("应同时出现别名与真实模型条目，实际 %v", ids(data))
	}
	if alias["alias"] != "workbuddy/deepseek-v4.1-flash" {
		t.Errorf("alias 字段 = %v，want 主力全路径", alias["alias"])
	}
	if alias["owned_by"] != "workbuddy" {
		t.Errorf("owned_by = %v，want 主力所在渠道 workbuddy", alias["owned_by"])
	}
	// 逐字段比对（除 id / alias 两个必然不同的字段）
	for k, v := range real {
		if k == "id" {
			continue
		}
		got, has := alias[k]
		if !has {
			t.Errorf("别名条目缺少字段 %q（必须逐字段继承）", k)
			continue
		}
		if !jsonEqual(got, v) {
			t.Errorf("字段 %q 不一致：别名 %v vs 主力 %v", k, got, v)
		}
	}
	// 反向：别名条目不得多出除 alias 之外的字段（不得编造能力）
	for k := range alias {
		if k == "alias" || k == "id" {
			continue
		}
		if _, has := real[k]; !has {
			t.Errorf("别名条目多出主力没有的字段 %q（不得编造）", k)
		}
	}
}

// TestModelsAliasEntryOrderedFirst 别名条目排在真实模型之前。
func TestModelsAliasEntryOrderedFirst(t *testing.T) {
	rules := []config.ModelFallbackRule{
		{From: "workbuddy/deepseek-v4.1-flash", To: []string{"workbuddy/other"}, Name: "fast"},
	}
	h := newModelsHandler(t, rules, []provider.ModelInfo{richModel()}, true)
	data := getModels(t, h)
	if len(data) < 2 {
		t.Fatalf("条目不足：%v", ids(data))
	}
	if data[0]["id"] != "fast" {
		t.Fatalf("别名条目应排在最前，实际顺序 %v", ids(data))
	}
}

// TestModelsAliasHiddenWithoutAccount 主力渠道未接入任何账号时别名不出现（沿用现有过滤）。
func TestModelsAliasHiddenWithoutAccount(t *testing.T) {
	rules := []config.ModelFallbackRule{
		{From: "workbuddy/deepseek-v4.1-flash", To: []string{"workbuddy/other"}, Name: "fast"},
	}
	h := newModelsHandler(t, rules, []provider.ModelInfo{richModel()}, false)
	if data := getModels(t, h); len(data) != 0 {
		t.Fatalf("渠道无账号时不应有任何条目，实际 %v", ids(data))
	}
}

// TestModelsAliasHotReload 别名表随降级链热更新即时生效（无需缓存失效通道）。
func TestModelsAliasHotReload(t *testing.T) {
	h := newModelsHandler(t, nil, []provider.ModelInfo{richModel()}, true)
	for _, e := range getModels(t, h) {
		if e["id"] == "fast" {
			t.Fatal("未配置别名时不应出现别名条目")
		}
	}
	h.SetModelFallbacks([]config.ModelFallbackRule{
		{From: "workbuddy/deepseek-v4.1-flash", To: []string{"workbuddy/other"}, Name: "fast"},
	})
	if data := getModels(t, h); data[0]["id"] != "fast" {
		t.Fatalf("热更新后应立刻出现别名条目，实际 %v", ids(data))
	}
	// 删规则 = 别名一起消失（不留孤儿条目）
	h.SetModelFallbacks(nil)
	for _, e := range getModels(t, h) {
		if e["id"] == "fast" {
			t.Fatal("删除规则后别名条目必须消失")
		}
	}
}

// TestModelsAliasSkipsUnknownPrimary 主力模型不在当前目录时不凭空造条目
// （别名是「主力模型的另一个名字」，主力自己都不在目录里就没有可继承的能力）。
func TestModelsAliasSkipsUnknownPrimary(t *testing.T) {
	rules := []config.ModelFallbackRule{
		{From: "workbuddy/not-in-catalog", To: []string{"workbuddy/other"}, Name: "fast"},
	}
	h := newModelsHandler(t, rules, []provider.ModelInfo{richModel()}, true)
	for _, e := range getModels(t, h) {
		if e["id"] == "fast" {
			t.Fatalf("主力不在目录里时不得凭空造别名条目：%v", ids(data0(h)))
		}
	}
}

// TestModelsNoAliasNoField 无别名时条目里不得出现 alias 字段（不改变既有输出形状）。
func TestModelsNoAliasNoField(t *testing.T) {
	h := newModelsHandler(t, nil, []provider.ModelInfo{richModel()}, true)
	for _, e := range getModels(t, h) {
		if _, has := e["alias"]; has {
			t.Fatalf("无别名时不得出现 alias 字段：%v", e)
		}
	}
}

// ---- 小工具 ----

func ids(data []map[string]any) []string {
	out := make([]string, 0, len(data))
	for _, e := range data {
		s, _ := e["id"].(string)
		out = append(out, s)
	}
	return out
}

func data0(h *Handler) []map[string]any {
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return resp.Data
}

// jsonEqual 比较两个 any 的 JSON 语义（map/slice 的 Go 值比较不可直接用 ==）。
func jsonEqual(a, b any) bool {
	ra, err1 := json.Marshal(a)
	rb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ra) == string(rb)
}
