package officeace

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wild-work/internal/provider"
)

// 锁住云端「按日消耗」端点的三件事：
//  1. 路径与查询串（聚合口径 = day，attr = officeace_points）
//  2. `x-subscription-type: v2` 头（漏了会被上游拒）与 V11 签名头确实发出去了
//  3. created_at（北京当日 00:00 的 UTC 表示）必须换算成**北京**自然日，不是 UTC 日
//
// 全部走 httptest，不碰真上游（真实调用会消耗账号额度与风控暴露）。
func TestSubscriptionUsageDaily(t *testing.T) {
	var gotPath, gotQuery, gotSubType, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotSubType = r.Header.Get("x-subscription-type")
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json;charset=utf8")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{
					"sku_attr_code": "officeace_points", "consumed_amount": "1220.57",
					"total_token": "52013762", "created_at": "2026-10-20T16:00:00.000Z",
				},
				{
					// 同一份量也可能是数字形式：两种都要吃得住
					"sku_attr_code": "officeace_points", "consumed_amount": 926.53,
					"total_token": 36210499, "created_at": "2026-10-19T16:00:00.000Z",
				},
				{
					// 别的 attr 不进本口径
					"sku_attr_code": "other_attr", "consumed_amount": "9",
					"total_token": "9", "created_at": "2026-10-19T16:00:00.000Z",
				},
				{
					// 没有时间的桶丢弃，不许当成今天
					"sku_attr_code": "officeace_points", "consumed_amount": "1",
					"total_token": "1", "created_at": "",
				},
			},
		})
	}))
	defer srv.Close()

	c := New()
	c.SetTransport(srv.Client().Transport)
	ac := &Account{BaseURL: srv.URL, AccessKeyID: "ak-test", SecretAccessKey: "sk-test"}

	buckets, err := c.subscriptionUsageDaily(ac)
	if err != nil {
		t.Fatalf("subscriptionUsageDaily: %v", err)
	}

	if gotPath != "/v1/subscription/usage" {
		t.Errorf("path = %q, want %q", gotPath, "/v1/subscription/usage")
	}
	if !strings.Contains(gotQuery, "sku_attr_code="+pointsAttrCode) {
		t.Errorf("query = %q, 缺 sku_attr_code=%s", gotQuery, pointsAttrCode)
	}
	if !strings.Contains(gotQuery, "aggregation="+usageAggregationDay) {
		t.Errorf("query = %q, 缺 aggregation=%s（缺了会拿不到按日桶）", gotQuery, usageAggregationDay)
	}
	if gotSubType != "v2" {
		t.Errorf("x-subscription-type = %q, want %q", gotSubType, "v2")
	}
	if !strings.Contains(gotAuth, "V11-HMAC-SHA256") {
		t.Errorf("Authorization = %q, want 含 V11-HMAC-SHA256", gotAuth)
	}

	if len(buckets) != 2 {
		t.Fatalf("桶数 = %d, want 2（其它 attr 与无时间桶都应被丢掉）: %+v", len(buckets), buckets)
	}
	want0 := UsageBucket{Day: "2026-10-20", Points: 926.53, Tokens: 36210499}
	if buckets[0] != want0 {
		t.Errorf("buckets[0] = %+v, want %+v", buckets[0], want0)
	}
	want1 := UsageBucket{Day: "2026-10-21", Points: 1220.57, Tokens: 52013762}
	if buckets[1] != want1 {
		t.Errorf("buckets[1] = %+v, want %+v（16:00Z 必须算成北京次日）", buckets[1], want1)
	}
	// 输出契约：**按 Day 升序**（不依赖上游回传顺序）——差分记账靠相邻两条做差，
	// 顺序不确定会直接把归因打乱，故不能「上游给什么顺序就什么顺序」。
	if !(buckets[0].Day < buckets[1].Day) {
		t.Errorf("桶未按 Day 升序：%+v", buckets)
	}
}

// created_at 回成 epoch 数字时不得整桶丢弃（旧实现只走 parseISO，与 numStr 的
// 「兼容数字形式」声明自相矛盾：串解不出来 → Day=="" → continue，静默少一天）。
func TestUsageDayOfNumericEpoch(t *testing.T) {
	cases := []struct{ in, want, why string }{
		{"1760976000000", "2025-10-21", "13 位 = 毫秒"},
		{"1760976000", "2025-10-21", "10 位 = 秒"},
		{"1760976000000 ", "2025-10-21", "尾随空白不得影响解析"},
		{"999", "", "3 位不像时间戳，不猜"},
		{"0", "", "零值不归属（否则全堆到 1970-01-01）"},
	}
	for _, c := range cases {
		if got := usageDayOf(c.in); got != c.want {
			t.Errorf("usageDayOf(%q) = %q, want %q（%s）", c.in, got, c.want, c.why)
		}
	}
	// 与串形式必须得到同一个自然日（两路解析不得分叉）。
	if usageDayOf("1760976000000") != usageDayOf("2025-10-20T16:00:00.000Z") {
		t.Errorf("数字/串两种 created_at 归属不一致：%q vs %q",
			usageDayOf("1760976000000"), usageDayOf("2025-10-20T16:00:00.000Z"))
	}
}

// 上游把同一自然日拆多条时必须合并（否则按日消耗被重复计入 ledger）。
func TestMergeUsageBuckets(t *testing.T) {
	got := mergeUsageBuckets([]UsageBucket{
		{Day: "2026-10-21", Points: 100, Tokens: 10},
		{Day: "2026-10-20", Points: 1, Tokens: 1},
		{Day: "2026-10-21", Points: 2.5, Tokens: 5},
	})
	want := []UsageBucket{
		{Day: "2026-10-20", Points: 1, Tokens: 1},
		{Day: "2026-10-21", Points: 102.5, Tokens: 15},
	}
	if len(got) != len(want) {
		t.Fatalf("合并后桶数 = %d, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if n := len(mergeUsageBuckets(nil)); n != 0 {
		t.Errorf("nil 输入应回空：%d", n)
	}
}

// 失败必须上抛：不能因为读不到消耗就把「今天消耗 0」写进账本。
func TestSubscriptionUsageDailyError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("OfficeAce.11020001"))
	}))
	defer srv.Close()

	c := New()
	c.SetTransport(srv.Client().Transport)
	ac := &Account{BaseURL: srv.URL, AccessKeyID: "ak-test", SecretAccessKey: "sk-test"}

	_, err := c.subscriptionUsageDaily(ac)
	if err == nil {
		t.Fatal("HTTP 500 时应返回 error，实际返回 nil")
	} else if !strings.Contains(err.Error(), "usage HTTP 500") {
		t.Errorf("err = %v, want 含 %q", err, "usage HTTP 500")
	}
	// 必须是**类型化**错误（不变量 19 / R79）：将来接上刷新与明细路径时，
	// app.refreshIfSessionDead 与 handler 的惩罚判定全靠 errors.As 取 Kind，
	// 裸 fmt.Errorf 会让 401 自愈永不生效。
	var pe *provider.Error
	if !errors.As(err, &pe) {
		t.Fatalf("应回 *provider.Error，实回 %T: %v", err, err)
	}
	if pe.Kind != provider.ErrServer || pe.Status != http.StatusInternalServerError {
		t.Errorf("Kind/Status = %v/%d, want %v/%d", pe.Kind, pe.Status,
			provider.ErrServer, http.StatusInternalServerError)
	}
}

// 401 归 ErrAccountFault 而非 ErrSessionDead（见 classifyUpstream 注释）：
// 网关 Basic 凭据不过期，误 Disable 会把用户不会手工恢复的账号永久哑掉。
func TestSubscriptionUsageDailyAuthFaultKind(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("APIG.1009 unauthorized"))
	}))
	defer srv.Close()

	c := New()
	c.SetTransport(srv.Client().Transport)
	ac := &Account{BaseURL: srv.URL, AccessKeyID: "ak-test", SecretAccessKey: "sk-test"}

	var pe *provider.Error
	if _, err := c.subscriptionUsageDaily(ac); !errors.As(err, &pe) {
		t.Fatalf("401 应回 *provider.Error，实回 %v", err)
	}
	if pe.Kind != provider.ErrAccountFault {
		t.Errorf("401 Kind = %v, want %v", pe.Kind, provider.ErrAccountFault)
	}
}

func TestUsageDayOf(t *testing.T) {
	cases := []struct{ in, want string }{
		{"2026-10-20T16:00:00.000Z", "2026-10-21"}, // 北京 10-21 的桶
		{"2026-10-21T00:00:00+08:00", "2026-10-21"},
		{"2026-10-19T16:00:00.000Z", "2026-10-20"},
		{"", ""},
		{"not-a-time", ""},
	}
	for _, c := range cases {
		if got := usageDayOf(c.in); got != c.want {
			t.Errorf("usageDayOf(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPickUsageDay(t *testing.T) {
	bs := []UsageBucket{
		{Day: "2026-10-20", Points: 926.53, Tokens: 36210499},
		{Day: "2026-10-21", Points: 1220.57, Tokens: 52013762},
	}
	if got := pickUsageDay(bs, "2026-10-21"); got.Points != 1220.57 || got.Tokens != 52013762 {
		t.Errorf("pickUsageDay 命中错桶：%+v", got)
	}
	got := pickUsageDay(bs, "2026-10-22")
	if got.Day != "2026-10-22" || got.Points != 0 || got.Tokens != 0 {
		t.Errorf("缺失日应回零值桶（Day 仍填该日）：%+v", got)
	}
	if got := pickUsageDay(nil, "2026-10-21"); got.Points != 0 || got.Day != "2026-10-21" {
		t.Errorf("空切片应回零值桶：%+v", got)
	}
}
