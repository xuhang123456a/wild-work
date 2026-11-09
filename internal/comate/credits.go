package comate

// credits.go —— comate 渠道的「实际积分/额度」与账号身份（R136）。
//
// 为什么有这个文件：R130/R135 期间 comate 被登记成 credits_na（上游「查不到余额」），
// 面板对 comate 账号一律显示「不适用」、也没有「刷新积分」入口。实际情况是
// **上游有额度端点，只是不在 llmproxy 的 /credits 家族里**（/credits /balance
// /userinfo 等候选确实全是 404，当初据此下了 NA 结论）。2026-10-29 实测找到：
//
//  1. GET /llmproxy/v1/user/token-usage（Cookie: wps_sid）→ code:0
//     data.credits.daily_limit {limit, remain, used}      每日额度（企业号 1000 / 已用 217 / 剩 1783）
//     data.credits_usage.periods[] {credits_limit, credits_rest, credits_used,
//     period_label("每日"/"每月"), period_type("daily"/"monthly")}
//     个人号（未开通 Comate）这边 daily_limit = null、periods 的值全是 -1 —— 即「没有额度」，
//     与 /models 空目录是同一件事的两个侧面。
//
//  2. GET https://comate.wps.cn/api/coserve/v1/users/current（Cookie: wps_sid）
//     → data.nickname + data.company_name + data.current_company_id（面板昵称用）
//
//  3. GET https://comate.wps.cn/api/coserve/v1/billing/v3/credits/trial（Cookie: wps_sid）
//     → data.trial_info {value, consumed, precision{value,consumed,scale}, expire_time}
//     **试用额度**：token-usage 里 daily_limit 为 null 的账号（如 PSY 企业号 642757510）
//     额度其实在这里 —— 实测 value 100.00 / consumed 5.24 ⇒ 剩余 94.76。
//     正好是客户端显示的「190」。这类账号的 periods 全是 -1，只看 token-usage 会误判成「没额度」。
//     数值要按 precision 走：JSON 里的整数值是缩放后的原始值（100000，scale=3），
//     precision.value 才是人读的 "100.00"。
//
// 口径：**额度只有每日这一种**（Comate 客户端显示的也正是「每日用量 1000」），
// 所以主积分 = 每日剩余，不掺任何别的池子。注意 WPS 账号级的「智点」
// （lingxi.kdocs.cn/api/public/v1/credits/balance）是**灵犀渠道**的口径，
// comate 不使用、也不展示 —— 混进来会让用户以为 comate 还有额度（实测个人号
// 智点 800 但 comate 一个模型都没有）。

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// 上游端点。
//
// ⚠️ 前缀不同，别拼错（R136 踩过）：EpTokenUsage 挂在 **APIBase**
// （`https://comate.wps.cn/llmproxy/v1/user`）下，与 /chat/completions、/models 同级；
// EpUserCurrent 在 comate **主域根**（`https://comate.wps.cn/api/coserve/v1/users/current`，
// 没有 llmproxy 前缀）；EpWPSBalance 在 WPS 账号级域（与 lingxi 渠道同源）。
// 把 EpTokenUsage 拼到主域根会拿到一页 HTML（`invalid character '<'`）。
const (
	EpTokenUsage   = "/token-usage"
	EpUserCurrent  = "/api/coserve/v1/users/current"
	EpTrialCredits = "/api/coserve/v1/billing/v3/credits/trial"

	// OriginComate 是 comate 主域（不含 llmproxy 前缀）。
	OriginComate = "https://comate.wps.cn"
)

const (
	// usageModeMixed 上游声明「额度与智点混合计费」（实测两个账号均为 mixed）。
	usageModeMixed = "mixed"
	// periodNoLimit 上游用 -1 表示该周期没有额度（不是 0，也不代表用光）。
	periodNoLimit = -1
	// profileTimeout 身份查询的独立超时（见 FetchProfile 注释）。
	profileTimeout = 8 * time.Second
)

// expireLoc 与 provider 包同一时区口径（UTC+8）：上游 expire_time 是 Unix 秒，
// 换算成人读时间必须按 WPS 的墙钟，否则面板上会差 8 小时。
var expireLoc = time.FixedZone("CST", 8*3600)

const ()

// creditQuota 是 data.credits.daily_limit 的形态（数值是**小数**，如 1857.37）。
type creditQuota struct {
	Limit  float64 `json:"limit"`
	Remain float64 `json:"remain"`
	Used   float64 `json:"used"`
}

// creditPeriod 是 data.credits_usage.periods[] 的条目。
type creditPeriod struct {
	Limit float64 `json:"credits_limit"`
	Rest  float64 `json:"credits_rest"`
	Used  float64 `json:"credits_used"`
	Label string  `json:"period_label"`
	Type  string  `json:"period_type"`
}

// tokenUsageData 是 /token-usage 的 data 段（其余字段与本渠道无关，不解析）。
type tokenUsageData struct {
	Credits struct {
		DailyLimit    *creditQuota `json:"daily_limit"`
		UsageMode     string       `json:"usage_mode"`
		MixedPriority string       `json:"mixed_priority"`
	} `json:"credits"`
	CreditsUsage struct {
		Periods []creditPeriod `json:"periods"`
	} `json:"credits_usage"`
}

// trialInfo 是 trial 额度条目。**数值一律以 precision 为准**：
// JSON 里 `value:100000` 是缩放后的整数（scale=3），precision.value 才是 "100.00"。
type trialInfo struct {
	PeriodType  string  `json:"period_type"`
	PeriodValue int     `json:"period_value"`
	Value       float64 `json:"value"`
	Consumed    float64 `json:"consumed"`
	ExpireTime  int64   `json:"expire_time"`
	Precision   struct {
		Value    string `json:"value"`
		Consumed string `json:"consumed"`
		Scale    int    `json:"scale"`
	} `json:"precision"`
}

// trialCreditsData 是 billing/v3/credits/trial 的 data 段。
type trialCreditsData struct {
	TrialAvailable bool       `json:"trial_available"`
	TrialInfo      *trialInfo `json:"trial_info"`
}

// trialAmounts 返回（总量, 已用, 剩余），单位是人读的积分。
// 优先 precision（字符串，已带小数）；缺 precision 时按 scale 还原原始整数。
func trialAmounts(t *trialInfo) (total, used, remain float64) {
	if t == nil {
		return 0, 0, 0
	}
	total = t.Value
	used = t.Consumed
	if v := strings.TrimSpace(t.Precision.Value); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			total = f
		}
	}
	if v := strings.TrimSpace(t.Precision.Consumed); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			used = f
		}
	}
	if t.Precision.Value == "" && t.Precision.Scale > 0 {
		div := math.Pow(10, float64(t.Precision.Scale))
		total, used = total/div, used/div
	}
	remain = total - used
	if remain < 0 {
		remain = 0
	}
	return total, used, remain
}

// absURL 拼绝对地址：getJSON 走 c.base()（llmproxy 前缀），本文件两个端点不在该前缀下。
func absURL(origin, path string) string {
	return strings.TrimRight(origin, "/") + path
}

// fetchJSON 发 GET 并返回 body（供跨域的 account/quota 端点复用）。
func (c *Client) fetchJSON(a *auth.Auth, url string) (json.RawMessage, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	applyHeaders(req, sidOf(a))
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("comate: %s HTTP %d: %s", url, resp.StatusCode, truncate(string(raw), 256))
	}
	return raw, nil
}

// fetchTokenUsage 拉额度信封。
func (c *Client) fetchTokenUsage(a *auth.Auth) (*tokenUsageData, error) {
	raw, err := c.fetchJSON(a, c.base()+EpTokenUsage) // 注意：挂 APIBase 下，不是主域根
	if err != nil {
		return nil, err
	}
	var env struct {
		Code int            `json:"code"`
		Data tokenUsageData `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("comate: token-usage 响应解析失败: %w", err)
	}
	// 业务错误压平成 100 是本渠道的既有形态（见包注释实测事实 2）。
	if env.Code != 0 {
		return nil, fmt.Errorf("comate: token-usage 业务错误 code=%d", env.Code)
	}
	return &env.Data, nil
}

// fetchTrial 拉「免费额度」（试用额度）信封。
//
// 失败按「没有试用额度」处理而不报错：这个端点只在带试用包的账号上才有内容，
// 老账号（673940726）实测 trial_available=false，不能因为它把额度刷新整体判失败。
func (c *Client) fetchTrial(a *auth.Auth) (*trialInfo, error) {
	raw, err := c.fetchJSON(a, absURL(c.originOf(), EpTrialCredits))
	if err != nil {
		return nil, err
	}
	var env struct {
		Code int              `json:"code"`
		Data trialCreditsData `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("comate: trial 响应解析失败: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("comate: trial 业务错误 code=%d", env.Code)
	}
	if !env.Data.TrialAvailable || env.Data.TrialInfo == nil {
		return nil, nil
	}
	return env.Data.TrialInfo, nil
}

// trialItem 把试用额度摊成 ResourceItem；没有试用额度时返回 nil。
func trialItem(t *trialInfo) *provider.ResourceItem {
	total, used, remain := trialAmounts(t)
	if total <= 0 {
		return nil
	}
	item := &provider.ResourceItem{
		Name:   "免费额度",
		Total:  int64(math.Round(total)),
		Used:   int64(math.Round(used)),
		Remain: int64(math.Round(remain)),
		Key:    "trial",
		Usable: true,
	}
	// expire_time 是 Unix 秒；转成 RFC3339 精确时刻（到期即失效，不是「当天全天有效」）。
	if t.ExpireTime > 0 {
		item.ExpireAt = time.Unix(t.ExpireTime, 0).In(expireLoc).Format(time.RFC3339)
	}
	return item
}

// quotaItems 把额度信封摊成 ResourceItem（每日/每月各一条，limit<=0 的周期跳过）。
// 每日额度带「当日有效」语义（ExpireAt=今天），与 provider 包对当日发放池的约定一致。
func quotaItems(d *tokenUsageData) ([]provider.ResourceItem, int64) {
	items := make([]provider.ResourceItem, 0, 3)
	primary := int64(0)
	for _, p := range d.CreditsUsage.Periods {
		if p.Limit <= 0 || p.Limit == periodNoLimit {
			continue // -1 = 该周期没有额度，不是「0 可用」
		}
		label := strings.TrimSpace(p.Label)
		if label == "" {
			label = p.Type
		}
		item := provider.ResourceItem{
			Name:   label + "额度",
			Total:  int64(math.Round(p.Limit)),
			Used:   int64(math.Round(p.Used)),
			Remain: int64(math.Round(p.Rest)),
			Key:    p.Type,
			Usable: true,
		}
		if p.Type == "daily" {
			item.ExpireAt = provider.ExpireDateToday()
			primary = item.Remain // 每日额度优先作为主积分
		}
		items = append(items, item)
	}
	// periods 缺失时退回 credits.daily_limit（两种下发形态并存，取到哪个用哪个）。
	if len(items) == 0 && d.Credits.DailyLimit != nil && d.Credits.DailyLimit.Limit > 0 {
		dl := d.Credits.DailyLimit
		items = append(items, provider.ResourceItem{
			Name:     "每日额度",
			Total:    int64(math.Round(dl.Limit)),
			Used:     int64(math.Round(dl.Used)),
			Remain:   int64(math.Round(dl.Remain)),
			ExpireAt: provider.ExpireDateToday(),
			Key:      "daily",
			Usable:   true,
		})
		primary = int64(math.Round(dl.Remain))
	}
	return items, primary
}

// UserResource 实现 provider.Upstream：返回**主积分**（每日额度剩余）。
//
// 语义约定：查不到额度时必须回中性值而不是 error —— `token-usage` 对未开通账号
// 回 `code:0` + 全 -1（个人号），那是「没有额度」而非「请求失败」，所以回 (0, nil)，
// 让面板显示 0，而不是报「刷新失败」。
func (c *Client) UserResource(a *auth.Auth) (int64, error) {
	d, err := c.fetchTokenUsage(a)
	if err != nil {
		return 0, err
	}
	_, primary := quotaItems(d)
	if primary > 0 {
		return primary, nil
	}
	// 每日额度为 0/-1 时回退到「免费额度」（试用包）：实测 PSY 企业号只有这一份额度。
	// 拉取失败不报错 —— 见 fetchTrial 注释。
	if t, terr := c.fetchTrial(a); terr == nil && t != nil {
		if _, _, remain := trialAmounts(t); remain > 0 {
			return int64(math.Round(remain)), nil
		}
	}
	return primary, nil
}

// UserResourceDetail 实现 provider.Upstream：额度周期明细（每日/每月）。
//
// 只有额度一个口径（不含「智点」，那是灵犀渠道的东西，见包注释）。
func (c *Client) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	d, err := c.fetchTokenUsage(a)
	if err != nil {
		return 0, nil, err
	}
	items, primary := quotaItems(d)
	// 免费额度独立成条（「通过活动获取、优先消耗」，与每日额度不是同一个池子）。
	// 主积分仍是「先用每日、再退免费额度」：两者都有的账号（PSY 只有免费额度）
	// 不能让免费额度把每日额度盖掉。
	if t, terr := c.fetchTrial(a); terr == nil && t != nil {
		if item := trialItem(t); item != nil {
			items = append(items, *item)
			if primary <= 0 {
				primary = item.Remain
			}
		}
	}
	return primary, items, nil
}

// FetchProfile 取账号身份：昵称 + 企业名（面板显示用，零 token 消耗）。
//
// 端点 `GET /api/coserve/v1/users/current`（Cookie wps_sid）实测回：
//
//	personal: {"user_id":224720862,"nickname":"dear life","current_company_id":0,...}
//	enterprise: {"user_id":1568907578,"nickname":"李柠希","company_name":"西南大学",
//	             "current_company_id":673940726,...}
//
// 为什么需要：`/models` 不回传任何身份字段（R130 注释），面板此前只能显示
// 「WPS 账号 2f4da9」这种 uid 尾号，多账号时根本分不清谁是谁。
//
// 超时单独压到 profileTimeout：这是**添加账号路径上的附加装饰**，
// 宁可拿不到昵称也不能让用户在「保存凭据」这一步干等 120s。
func FetchProfile(sid string) (nickname, company string, err error) {
	cli := New()
	cli.HTTP.Timeout = profileTimeout
	return cli.fetchProfile(&auth.Auth{AccessToken: sid})
}

func (c *Client) fetchProfile(a *auth.Auth) (nickname, company string, err error) {
	raw, err := c.fetchJSON(a, absURL(c.originOf(), EpUserCurrent))
	if err != nil {
		return "", "", err
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Nickname    string `json:"nickname"`
			CompanyName string `json:"company_name"`
			UserID      int64  `json:"user_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", "", fmt.Errorf("comate: users/current 响应解析失败: %w", err)
	}
	if env.Code != 0 {
		return "", "", fmt.Errorf("comate: users/current 业务错误 code=%d", env.Code)
	}
	return strings.TrimSpace(env.Data.Nickname), strings.TrimSpace(env.Data.CompanyName), nil
}

// DisplayName 组装面板昵称：有企业名则「昵称（企业名）」，否则昵称；
// 两者都取不到时返回空串，调用方回退到 uid 尾号占位。
func DisplayName(nickname, company string) string {
	nickname = strings.TrimSpace(nickname)
	company = strings.TrimSpace(company)
	switch {
	case nickname != "" && company != "":
		return nickname + "（" + company + "）"
	case nickname != "":
		return nickname
	case company != "":
		return company
	default:
		return ""
	}
}
