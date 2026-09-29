// handler_cached_models_test.go 回归测试：费率面板的模型缓存读取策略（issue #40）。
//
// 历史缺陷：CachedChannelModels 在缓存过期后回退 StaticModels。对无静态兜底表的
// 渠道（qodercn/qodercom）这等于返回 nil → buildFeesChannels 整组跳过 →
// 面板「模型列表和费率」里该渠道整组消失（issue #40 截图：只剩 OpenCodeZen）。
//
// 锁定两个行为：
//  1. 缓存过期但曾成功拉取 → 沿用过期数据（陈旧比空好），不再被静态表覆盖；
//  2. 从未成功拉取且无静态表 → 返回空（渠道整组隐藏，属 fail-fast 既有语义）。
package server

import (
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// cachedModelsUpstream 动态模型表可注入的假上游。
type cachedModelsUpstream struct {
	echoUpstream
	infos []provider.ModelInfo
	err   error
}

func (u *cachedModelsUpstream) FetchModels(*auth.Auth) ([]provider.ModelInfo, error) {
	return u.infos, u.err
}

func (u *cachedModelsUpstream) FetchModelPricing(*auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}

// newCacheTestHandler 构造一个只挂 kind 渠道、账号已绑定的 handler。
func newCacheTestHandler(t *testing.T, kind provider.Kind, up provider.Upstream) (*Handler, *pool.Pool) {
	t.Helper()
	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at"}
	p.Add(a)
	h := NewHandler(Config{Runtimes: map[provider.Kind]*Runtime{
		kind: {Kind: kind, Pool: p, Upstream: up},
	}})
	return h, p
}

// TestCachedChannelModelsPrefersStaleOverEmptyStatic 缓存过期但曾成功拉取：
// 必须沿用过期数据，而不是被静态兜底表（无静态表时为 nil）覆盖。
// 修复前：过 TTL 即回 StaticModels → qodercn 分组从面板消失（issue #40）。
func TestCachedChannelModelsPrefersStaleOverEmptyStatic(t *testing.T) {
	up := &cachedModelsUpstream{infos: []provider.ModelInfo{
		{ID: "m1", ContextFromAPI: true},
		{ID: "m2", ContextFromAPI: true},
	}}
	h, _ := newCacheTestHandler(t, provider.QoderCN, up)

	// 先触发一次成功拉取，填充 rt.models
	if got := h.ChannelModels()[provider.QoderCN]; len(got) != 2 {
		t.Fatalf("首次拉取应得 2 个模型, got %d", len(got))
	}

	// 人为把缓存打旧（跨过 dynamicModelsTTL=1h）
	rt := h.cfg.Runtimes[provider.QoderCN]
	rt.mu.Lock()
	rt.fetched = time.Now().Add(-2 * time.Hour)
	rt.mu.Unlock()

	got := h.CachedChannelModels()[provider.QoderCN]
	if len(got) != 2 {
		t.Fatalf("缓存过期后应沿用上次成功结果（issue #40），got %d 条", len(got))
	}
	if got[0].ID != "m1" || got[1].ID != "m2" {
		t.Errorf("过期缓存内容被篡改: %+v", got)
	}
}

// TestCachedChannelModelsFallsBackToStaticWhenNeverFetched 从未成功拉取：
// 回退静态兜底表；静态表也为空时返回空（面板据此隐藏该渠道，fail-fast 语义不变）。
func TestCachedChannelModelsFallsBackToStaticWhenNeverFetched(t *testing.T) {
	up := &cachedModelsUpstream{infos: nil, err: nil}
	h, _ := newCacheTestHandler(t, provider.QoderCN, up) // qodercn 无静态表

	got := h.CachedChannelModels()[provider.QoderCN]
	if len(got) != 0 {
		t.Errorf("从未拉取且无静态表时应返回空, got %d 条", len(got))
	}
}

// TestCachedChannelModelsFallsBackToStaticList 有静态表的渠道：
// 从未拉取时回退静态表（既有行为不变）。
func TestCachedChannelModelsFallsBackToStaticList(t *testing.T) {
	up := &cachedModelsUpstream{infos: nil}
	// 静态表需显式设置：NewHandler 只在默认兑底路径才自动注入
	h, _ := newCacheTestHandler(t, provider.TraeWork, up)
	rt := h.cfg.Runtimes[provider.TraeWork]
	rt.StaticModels = []provider.ModelInfo{{ID: "static-1"}, {ID: "static-2"}}

	got := h.CachedChannelModels()[provider.TraeWork]
	if len(got) != 2 || got[0].ID != "static-1" {
		t.Fatalf("有静态表的渠道从未拉取时应回退静态表, got %+v", got)
	}
}
