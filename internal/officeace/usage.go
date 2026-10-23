package officeace

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// 云端「按日消耗」（积分 + Token）。
//
// 背景：`/v1/subscription` 返回的是**套餐存量快照**（total / current），它不随用量实时变化——
// 用它看不出「今天烧了多少」。真正的消耗明细在上游另一个订阅端点：
//
//	GET {gateway}/v1/subscription/usage?sku_attr_code=officeace_points&aggregation=day
//
// 该端点与额度端点同属订阅控制面（同样**不带 /v2**），签名方式一致（V11），并要求
// `x-subscription-type: v2` 头——这一点由 doV11 统一注入，故此处无需重复。
//
// 响应形如：
//
//	{"items":[{"sku_attr_code":"officeace_points","consumed_amount":"1220.57",
//	            "total_token":"52013762","created_at":"2026-10-20T16:00:00.000Z"}]}
//
// ★created_at 是「北京当日 00:00」的 **UTC 表示**（`2026-10-20T16:00:00.000Z` = 北京 10-21），
// 因此必须先换到 +08 时区再取日期，否则整桶会被算成前一天。
const usageAggregationDay = "day"

// UsageBucket 是「一天」的消耗汇总（按北京时间自然日切分）。
type UsageBucket struct {
	Day    string  // 北京时间的自然日，形如 2026-10-21
	Points float64 // 当日消耗积分
	Tokens int64   // 当日消耗 token
}

// SubscriptionUsageDaily 拉取云端按日消耗（上游给出多少天就返回多少天）。
// 与 UserResource 同纪律：**失败一律上抛**，绝不返回零值假装「今天没消耗」。
//
// 输出契约（给 ledger / 面板消费，务必确定）：**按 Day 升序**、**同一自然日已合并为一条**，
// 上游若把一天拆成多条（分页残留 / 同 sku 多次回补）不会让消费方重复计数。
func (c *Client) SubscriptionUsageDaily(a *auth.Auth) ([]UsageBucket, error) {
	ac := c.ensureCredential(a)
	return c.subscriptionUsageDaily(ac)
}

// SubscriptionUsageToday 取「今天（北京时间）」那一桶；今天还没产生消耗时返回零值桶。
func (c *Client) SubscriptionUsageToday(a *auth.Auth) (UsageBucket, error) {
	buckets, err := c.SubscriptionUsageDaily(a)
	if err != nil {
		return UsageBucket{}, err
	}
	return pickUsageDay(buckets, time.Now().In(expireLoc).Format("2006-01-02")), nil
}

func (c *Client) subscriptionUsageDaily(ac *Account) ([]UsageBucket, error) {
	gw, err := c.gatewayBase(ac)
	if err != nil {
		return nil, err
	}
	u := trimSlash(gw) + "/v1/subscription/usage?sku_attr_code=" + pointsAttrCode +
		"&aggregation=" + usageAggregationDay
	body, status, err := c.doV11(ac, http.MethodGet, u, "", "")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		// 不变量 19：上游 ≥400 必须回类型化 *provider.Error，裸 fmt.Errorf 会让调用方
		// errors.As 取不到 Kind ⇒ 将来接上 ledger/app 刷新路径时 401 自愈永不生效。
		// 本渠道 401/403 归 ErrAccountFault（见 classifyUpstream 注释），不会误 Disable 账号。
		return nil, &provider.Error{
			Kind:   classifyUpstream(status, string(body)),
			Status: status,
			Msg:    fmt.Sprintf("officeace: usage HTTP %d: %s", status, truncate(body, 200)),
		}
	}
	var resp usageResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	out := make([]UsageBucket, 0, len(resp.Items))
	for _, it := range resp.Items {
		if code := strings.TrimSpace(string(it.SKUAttrCode)); code != "" && code != pointsAttrCode {
			continue
		}
		b := UsageBucket{
			Day:    usageDayOf(string(it.CreatedAt)),
			Points: atofSafe(string(it.ConsumedAmount)),
			Tokens: atoiSafe(string(it.TotalToken)),
		}
		if b.Day == "" {
			continue // 没有时间的桶无法归属到某天，丢弃而不是塞进今天
		}
		out = append(out, b)
	}
	return mergeUsageBuckets(out), nil
}

// mergeUsageBuckets 按自然日合并并升序排序（同一天多条 ⇒ 积分/token 相加，只留一条）。
func mergeUsageBuckets(in []UsageBucket) []UsageBucket {
	byDay := make(map[string]*UsageBucket, len(in))
	for _, b := range in {
		if p := byDay[b.Day]; p != nil {
			p.Points += b.Points
			p.Tokens += b.Tokens
			continue
		}
		cp := b
		byDay[b.Day] = &cp
	}
	out := make([]UsageBucket, 0, len(byDay))
	for _, p := range byDay {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day < out[j].Day })
	return out
}

// pickUsageDay 从日桶里挑出指定北京自然日；缺失时返回该日的零值桶。
func pickUsageDay(buckets []UsageBucket, day string) UsageBucket {
	for _, b := range buckets {
		if b.Day == day {
			return b
		}
	}
	return UsageBucket{Day: day}
}

// usageDayOf 把上游 created_at（北京当日 00:00 的 UTC 表示）换算成北京自然日。
//
// 两种形态都要吃得住：实测下发 ISO 串（`2026-10-20T16:00:00.000Z`），但 numStr 已声明
// 本渠道同族接口偶有**数字**形式（epoch 毫秒/秒）——旧实现只走 parseISO，一旦 created_at
// 真回成数字，parseISO 失败 ⇒ 整桶被静默丢弃（与 numStr 的兼容声明自相矛盾）。
// 13 位按毫秒、10 位按秒判，其余长度不猜（回 "" 交给上层丢弃）。
func usageDayOf(s string) string {
	if ms, ok := parseISO(s); ok {
		return time.UnixMilli(ms).In(expireLoc).Format("2006-01-02")
	}
	if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil && n > 0 {
		switch {
		case n >= 1_000_000_000_000: // epoch 毫秒
			return time.UnixMilli(n).In(expireLoc).Format("2006-01-02")
		case n >= 1_000_000_000: // epoch 秒
			return time.Unix(n, 0).In(expireLoc).Format("2006-01-02")
		}
	}
	return ""
}

func atofSafe(s string) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v
}

type usageResponse struct {
	Items []usageItem `json:"items"`
}

type usageItem struct {
	SKUAttrCode    numStr `json:"sku_attr_code"`
	ConsumedAmount numStr `json:"consumed_amount"`
	TotalToken     numStr `json:"total_token"`
	CreatedAt      numStr `json:"created_at"`
}

// numStr 兼容上游把同一个量下发成「字符串」或「数字」两种形式
// （实测 consumed_amount / total_token 是字符串，但同族接口偶有数字形式，两种都要吃得住）。
type numStr string

func (n *numStr) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	s = strings.Trim(s, "\"")
	if s == "" || s == "null" {
		*n = ""
		return nil
	}
	*n = numStr(s)
	return nil
}
