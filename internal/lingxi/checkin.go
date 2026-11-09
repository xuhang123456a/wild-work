package lingxi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// checkin.go 灵犀签到与余额（internal/lingxi 包）。
//
// 端点（实测事实 6/7）：
//   - 签到任务查询 GET /api/public/v1/tasks → data.tasks[]（task_key=daily_check_in）
//   - 签到领取   POST /api/public/v1/tasks/daily_check_in/claim?date=YYYY-MM-DD（query 传日期、无 body）
//   - 余额查询   GET /api/public/v1/credits/balance → data.total_balance
//
// 失效判定：tasks 响应 result=="UserUnauthorized" 或 balance HTTP 401。

// tasksResponse GET /api/public/v1/tasks 响应信封。
type tasksResponse struct {
	Result string `json:"result"`
	Data   struct {
		Tasks []dailyTask `json:"tasks"`
	} `json:"data"`
}

type dailyTask struct {
	TaskKey      string `json:"task_key"`
	Status       string `json:"status"`
	RewardAmount int64  `json:"reward_amount"`
	Extra        struct {
		ConsecutiveDays int64 `json:"consecutive_days"`
		TodayReward     int64 `json:"today_reward"`
	} `json:"extra"`
}

// claimResponse POST claim 响应信封。
type claimResponse struct {
	Result string `json:"result"`
	Data   struct {
		Extra struct {
			TodayReward int64 `json:"today_reward"`
		} `json:"extra"`
		TotalClaimedCredits int64 `json:"total_claimed_credits"`
	} `json:"data"`
}

// balanceResponse GET /api/public/v1/credits/balance 响应信封。
type balanceResponse struct {
	Result string `json:"result"`
	Data   struct {
		TotalBalance int64  `json:"total_balance"`
		Balance      int64  `json:"balance"`
		BonusSummary struct {
			TotalValue     int64 `json:"total_value"`
			TotalConsumed int64 `json:"total_consumed"`
		} `json:"bonus_summary"`
		BonusCredits []bonusCredit `json:"bonus_credits"`
	} `json:"data"`
}

type bonusCredit struct {
	Balance    int64  `json:"balance"`
	ExpireTime string `json:"expire_time"`
}

// expireLoc 智点到期/签到日期的固定时区（东八区，与 provider 包一致）。
var expireLoc = time.FixedZone("UTC+8", 8*60*60)

// fetchDailyTask 查询每日签到任务：返回任务详情与是否存在。
func (c *Client) fetchDailyTask(a *auth.Auth) (*dailyTask, bool, error) {
	raw, err := c.getJSON(a, EpTasks)
	if err != nil {
		return nil, false, err
	}
	var resp tasksResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, false, fmt.Errorf("lingxi: tasks 响应解析失败: %w", err)
	}
	if resp.Result == "UserUnauthorized" {
		return nil, false, fmt.Errorf("lingxi: 凭据失效（UserUnauthorized）")
	}
	for i := range resp.Data.Tasks {
		if resp.Data.Tasks[i].TaskKey == "daily_check_in" {
			return &resp.Data.Tasks[i], true, nil
		}
	}
	return nil, false, nil // 无签到活动
}

// DailyCheckin 实现 provider.Upstream：领每日签到智点。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	_, err := c.dailyCheckinReport(a)
	return err
}

// DailyCheckinReport 实现 provider.CheckinReporter：签到结果带详情
// （奖励数值、连签天数、当前总智点），面板签到页展示用。
func (c *Client) DailyCheckinReport(a *auth.Auth) (provider.CheckinReport, error) {
	return c.dailyCheckinReport(a)
}

// dailyCheckinReport 签到主流程：查任务 → 已签返回 → 未签领取。
func (c *Client) dailyCheckinReport(a *auth.Auth) (provider.CheckinReport, error) {
	task, found, err := c.fetchDailyTask(a)
	if err != nil {
		return provider.CheckinReport{Status: provider.CheckinError}, err
	}
	if !found {
		return provider.CheckinReport{Status: provider.CheckinNoCampaign}, nil
	}
	if task.Status == "claimed" {
		return provider.CheckinReport{
		Status: provider.CheckinAlready,
		Msg:    fmt.Sprintf("今日已签到，已连签 %d 天", task.Extra.ConsecutiveDays),
	}, nil
	}

	// 领取：POST claim?date=今天（东八区日期，与上游对齐）。
	today := time.Now().In(expireLoc).Format("2006-01-02")
	req, err := http.NewRequest(http.MethodPost,
		c.base()+EpClaim+"?date="+today, nil)
	if err != nil {
		return provider.CheckinReport{Status: provider.CheckinError}, err
	}
	applyHeaders(req, sidOf(a))
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return provider.CheckinReport{Status: provider.CheckinError}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return provider.CheckinReport{Status: provider.CheckinError},
			fmt.Errorf("lingxi: 签到领取失败 HTTP %d: %s", resp.StatusCode, truncate(string(raw), 256))
	}
	var claim claimResponse
	if err := json.Unmarshal(raw, &claim); err != nil {
		return provider.CheckinReport{Status: provider.CheckinError},
			fmt.Errorf("lingxi: claim 响应解析失败: %w", err)
	}
	if claim.Result == "UserUnauthorized" {
		return provider.CheckinReport{Status: provider.CheckinError},
			fmt.Errorf("lingxi: 凭据失效（UserUnauthorized）")
	}
	if claim.Result != "ok" {
		// 非 ok（含已领幂等）：复查任务状态定性。
		if task2, _, err2 := c.fetchDailyTask(a); err2 == nil && task2 != nil && task2.Status == "claimed" {
			return provider.CheckinReport{
				Status: provider.CheckinAlready,
				Msg:    fmt.Sprintf("今日已签到，已连签 %d 天", task2.Extra.ConsecutiveDays),
			}, nil
		}
		return provider.CheckinReport{Status: provider.CheckinError},
			fmt.Errorf("lingxi: 签到领取失败: %s", claim.Result)
	}

	reward := claim.Data.Extra.TodayReward
	if reward == 0 {
		reward = task.RewardAmount
	}
	// claim 响应无权威连签字段（只有 today_reward / total_claimed_credits），
	// 连签天数用 claim 前查询到的 consecutive_days+1 推算展示——若签到前当天
	// 连签已断链，展示数可能与上游实际差 1（magpie 同款行为；2026-10-29 审查 P4）。
	return provider.CheckinReport{
		Status: provider.CheckinClaimed,
		Msg:    fmt.Sprintf("签到成功 +智点 %d，已连签 %d 天", reward, task.Extra.ConsecutiveDays+1),
		Amount: reward,
	}, nil
}

// UserResource 实现 provider.Upstream：返回可用智点（余额端点）。
func (c *Client) UserResource(a *auth.Auth) (int64, error) {
	usable, _, err := c.fetchBalance(a)
	return usable, err
}

// UserResourceDetail 实现 provider.Upstream：智点余额 + 基础/赠送包明细。
func (c *Client) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	return c.fetchBalance(a)
}

// fetchBalance 拉余额信封并组装 ResourceItem 列表（基础智点 + 赠送包）。
func (c *Client) fetchBalance(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	raw, err := c.getJSON(a, EpBalance)
	if err != nil {
		return 0, nil, err // getJSON 已把 401 带进错误文案
	}
	var resp balanceResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return 0, nil, fmt.Errorf("lingxi: balance 响应解析失败: %w", err)
	}
	if resp.Result == "UserUnauthorized" {
		return 0, nil, fmt.Errorf("lingxi: 凭据失效（UserUnauthorized）")
	}

	total := resp.Data.TotalBalance
	if total == 0 {
		total = resp.Data.Balance
	}

	// 赠送智点包（balance>0），到期时间规整为 YYYY-MM-DD。
	items := make([]provider.ResourceItem, 0, len(resp.Data.BonusCredits))
	for _, bc := range resp.Data.BonusCredits {
		if bc.Balance <= 0 {
			continue
		}
		expiry := bc.ExpireTime
		if t, err := time.Parse(time.RFC3339, bc.ExpireTime); err == nil {
			expiry = t.In(expireLoc).Format("2006-01-02")
		}
		items = append(items, provider.ResourceItem{
			Name:     "赠送智点",
			Total:    bc.Balance,
			Remain:   bc.Balance,
			ExpireAt: expiry,
			Key:      "bonus",
			Usable:   true,
		})
	}
	// 基础智点 = 总额 − 赠送合计（0 或负则不列，全并入赠送展示）。
	base := total
	for _, bc := range resp.Data.BonusCredits {
		if bc.Balance > 0 {
			base -= bc.Balance
		}
	}
	all := make([]provider.ResourceItem, 0, len(items)+1)
	if base > 0 {
		all = append(all, provider.ResourceItem{
			Name:   "基础智点",
			Total:  base,
			Remain: base,
			Key:    "base",
			Usable: true,
		})
	}
	all = append(all, items...)

	usable, _ := provider.Summarize(all)
	return usable, all, nil
}

// 确认实现签到上报接口（编译期断言）。
var _ provider.CheckinReporter = (*Client)(nil)
