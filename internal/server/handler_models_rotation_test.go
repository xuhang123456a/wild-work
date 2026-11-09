// handler_models_rotation_test.go 回归测试（R133）：模型目录是渠道维度的，
// 一次账号抽样失败不得让整条渠道的模型面归零。
//
// 实测事故：WPS Comate 池里加入第二个未开通 Comate 的账号后，该账号的
// /models 返回 items=[]（FetchModels 返回错误），若这次 Pick 恰好抽中它，
// 整条渠道 9 个模型瞬间变 0、面板里渠道整组消失，并进入 5 分钟负缓存，
// 直到负缓存过期或重启进程才恢复。
//
// 锁定三个行为：
//  1. 首个账号拉不到目录（错误或空）→ 自动换下一个账号，拿到目录即成功；
//  2. 所有候选账号都拉不到 → 返回 nil 并记 lastFail（负缓存语义不变）；
//  3. 换号上限受 maxModelsFetchAccounts 约束，不会把整池串行探完。
package server

import (
	"errors"
	"sync"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// perAccountModelsUpstream 按 uid 返回不同的目录结果，用于模拟
// 「同渠道里既有能给出目录的账号、也有未开通/失效的账号」。
type perAccountModelsUpstream struct {
	echoUpstream
	mu    sync.Mutex
	byUID map[string]modelCatalogResult
	calls []string
}

type modelCatalogResult struct {
	infos []provider.ModelInfo
	err   error
}

func (u *perAccountModelsUpstream) FetchModels(a *auth.Auth) ([]provider.ModelInfo, error) {
	u.mu.Lock()
	u.calls = append(u.calls, a.UID)
	u.mu.Unlock()
	if r, ok := u.byUID[a.UID]; ok {
		return r.infos, r.err
	}
	return nil, errors.New("unknown uid")
}

func (u *perAccountModelsUpstream) FetchModelPricing(*auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}

func (u *perAccountModelsUpstream) callUIDs() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.calls...)
}

// newRotationTestHandler 构造带指定账号的 handler。ExpiresAt 全 0：
// pool 的排序退化为「uid ASCII 升序」，首个被 Pick 的账号因此是确定的。
func newRotationTestHandler(t *testing.T, up provider.Upstream, uids ...string) (*Handler, *pool.Pool) {
	t.Helper()
	p := pool.New("")
	for _, uid := range uids {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at-" + uid})
	}
	h := NewHandler(Config{Runtimes: map[provider.Kind]*Runtime{
		provider.Comate: {Kind: provider.Comate, Pool: p, Upstream: up},
	}})
	return h, p
}

// TestFetchModelsRotatesPastEmptyAccount 首个账号目录为空（Comate 未开通），
// 必须换号重试并最终拿到目录；渠道模型面不得归零。
func TestFetchModelsRotatesPastEmptyAccount(t *testing.T) {
	up := &perAccountModelsUpstream{byUID: map[string]modelCatalogResult{
		// uid 升序靠前者：空目录 + 错误（comate 空目录被 FetchModels 转成错误）
		"a-unprovisioned": {err: errors.New("comate: 该账号没有可用模型")},
		"b-healthy": {infos: []provider.ModelInfo{
			{ID: "glm-5.3", Name: "GLM-5.3", ContextFromAPI: true},
			{ID: "deepseek-v4-flash", Name: "DeepSeek V4 Flash", ContextFromAPI: true},
		}},
	}}
	h, _ := newRotationTestHandler(t, up, "a-unprovisioned", "b-healthy")

	got := h.ChannelModels()[provider.Comate]
	if len(got) != 2 {
		t.Fatalf("首个账号拉不到目录时应换号重试，期望 2 个模型，got %d（%+v）", len(got), got)
	}
	if calls := up.callUIDs(); len(calls) != 2 || calls[0] != "a-unprovisioned" || calls[1] != "b-healthy" {
		t.Errorf("应先试 a-unprovisioned 再试 b-healthy，实际调用序列 %v", calls)
	}
	// 渠道缓存必须已被成功结果填充（而不是停留在负缓存路径）
	rt := h.cfg.Runtimes[provider.Comate]
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	if len(rt.models) != 2 {
		t.Errorf("成功目录应写入 rt.models，got %d", len(rt.models))
	}
	if !rt.lastFail.IsZero() {
		t.Error("换号成功后不应留下 lastFail（否则下一次会被负缓存挡住）")
	}
}

// TestFetchModelsAllAccountsFailSetsNegativeCache 所有候选都失败：
// 维持既有语义——返回 nil 并记录 lastFail 触发 5 分钟负缓存。
func TestFetchModelsAllAccountsFailSetsNegativeCache(t *testing.T) {
	up := &perAccountModelsUpstream{byUID: map[string]modelCatalogResult{
		"a": {err: errors.New("401 not_login")},
		"b": {err: nil}, // 200 但空目录
	}}
	h, _ := newRotationTestHandler(t, up, "a", "b")

	if got := h.ChannelModels()[provider.Comate]; len(got) != 0 {
		t.Fatalf("全部账号失败时应返回空（面板隐藏该渠道），got %d 条", len(got))
	}
	rt := h.cfg.Runtimes[provider.Comate]
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	if rt.lastFail.IsZero() {
		t.Error("全部账号失败必须记录 lastFail（负缓存 5 分钟，避免每请求打上游）")
	}
	if len(rt.models) != 0 {
		t.Errorf("失败不应写入模型缓存，got %d 条", len(rt.models))
	}
}

// TestFetchModelsRotationIsBounded 换号有上限：坏账号再多也不会把整池串行探完。
func TestFetchModelsRotationIsBounded(t *testing.T) {
	uids := []string{"a", "b", "c", "d", "e", "f"}
	up := &perAccountModelsUpstream{byUID: map[string]modelCatalogResult{}}
	for _, uid := range uids {
		up.byUID[uid] = modelCatalogResult{err: errors.New("unprovisioned")}
	}
	h, p := newRotationTestHandler(t, up, uids...)
	if n := len(p.List()); n != len(uids) {
		t.Fatalf("测试前置：池内应有 %d 个账号，got %d", len(uids), n)
	}

	_ = h.ChannelModels()[provider.Comate]

	calls := up.callUIDs()
	if len(calls) != maxModelsFetchAccounts {
		t.Fatalf("换号应被 maxModelsFetchAccounts=%d 限制，实际调用 %d 次：%v",
			maxModelsFetchAccounts, len(calls), calls)
	}
	// 上限之内不得重复试同一个账号
	seen := map[string]bool{}
	for _, uid := range calls {
		if seen[uid] {
			t.Fatalf("同一账号被重复探测：%v", calls)
		}
		seen[uid] = true
	}
}

// TestFetchModelsKeepsFreshCacheWhenHealthy 健康缓存仍在 TTL 内时不得打上游
// （换号逻辑只在真正需要重拉时才生效，不能把 TTL 破坏掉）。
func TestFetchModelsKeepsFreshCacheWhenHealthy(t *testing.T) {
	up := &perAccountModelsUpstream{byUID: map[string]modelCatalogResult{
		"a": {infos: []provider.ModelInfo{{ID: "m1", ContextFromAPI: true}}},
	}}
	h, _ := newRotationTestHandler(t, up, "a")

	if got := h.ChannelModels()[provider.Comate]; len(got) != 1 {
		t.Fatalf("首次拉取应得 1 个模型，got %d", len(got))
	}
	before := len(up.callUIDs())

	// TTL 内再读：应命中缓存，不再打上游
	if got := h.ChannelModels()[provider.Comate]; len(got) != 1 {
		t.Fatalf("TTL 内应命中缓存，got %d", len(got))
	}
	if after := len(up.callUIDs()); after != before {
		t.Errorf("TTL 内不应再拉上游：调用次数 %d → %d", before, after)
	}

	// 打旧缓存后重拉：仍走正常路径
	rt := h.cfg.Runtimes[provider.Comate]
	rt.mu.Lock()
	rt.fetched = time.Now().Add(-2 * time.Hour)
	rt.mu.Unlock()
	if got := h.ChannelModels()[provider.Comate]; len(got) != 1 {
		t.Fatalf("缓存过期后重拉应得 1 个模型，got %d", len(got))
	}
	if after := len(up.callUIDs()); after != before+1 {
		t.Errorf("缓存过期后应重拉一次：%d → %d", before, after)
	}
}
