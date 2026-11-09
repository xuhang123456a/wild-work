// Package lingxi 是 WPS 灵犀（lingxi.kdocs.cn 网页版助手）渠道的上游实现。
//
// 协议来源：本项目逆向参考了 [KevinFX-code/magpie-wps-lingxi]
// (https://github.com/KevinFX-code/magpie-wps-lingxi)（MIT）—— 一个把 WPS 灵犀
// 接入 Magpie/OpenCode 的插件。其对灵犀网关的鉴权形态（Cookie wps_sid + 三专用头）、
// 会话生命周期（每请求 POST sessions 新建 session）、消息压缩（多轮拼 [System:…]/User:/Assistant:
// 前缀）、reasoning_effort 档位映射（low→"0"/high→"1"/max→"2"）、SSE 帧型
//（event:text/data:{type,…}）与签到/余额端点（/api/public/v1/tasks、
// /api/public/v1/credits/balance）的知识为本渠道的逆向依据；
// 本项目只取协议知识，其 Magpie 插件封装、本地 callback server、模型注册表未采用。
//
// 凭据：WPS 账号的 **wps_sid Cookie**（lingxi.kdocs.cn 登录后浏览器 Cookie 里的
// wps_sid；与 comate.wps.cn 同一账号体系但**门禁独立**——同一 wps_sid 灵犀侧
// 开通即可用，comate 侧未开通则 /models 为空，2026-10-28 实测）。由面板
// 「粘贴 wps_sid」写入 auths/（见 app.SubmitLingxiSID）。Cookie 不轮换 ⇒
// 无 RefreshToken、无保活；但灵犀有**每日签到**（领智点）与**余额**端点。
//
// 渠道定位：粘贴型多账号渠道（与 comate 同形态），模型面来自上游
// /api/aioffice/v1/sessions/plans 的 data.models 数组原样透传（不改名、不过滤），
// 拉取失败回退 knownIDs 缓存（R42 同款）。
//
// 必须知道的实测事实（2026-10-28，经真实 wps_sid 直测 lingxi.kdocs.cn，
// 见 .trae/specs/灵犀渠道接入/notes.md 与 script_temp/probe_lingxi_*.py）：
//  1. **鉴权只认 Cookie**：Cookie: wps_sid=<sid>；与 comate 一致，Bearer 无效。
//     会话失效的权威形态：tasks 端点回 result:"UserUnauthorized"，
//     balance 端点回 HTTP 401。
//  2. **每请求新建 session**：POST /api/aigc/v3/assistant/sessions（body "{}"）
//     拿 data.session_id，再 POST .../sessions/<sid>/completions。
//     同一 session 并发会回 error 帧，故每请求各建一个会话（magpie 同款）。
//  3. **completions 三专用头**：X-Cc-Version:1 / X-Client-Type:lingxi-web /
//     X-Cc-Entrance:default；缺失时上游可能拒绝。
//  4. **SSE 帧型非 OpenAI 形**：event: 行 + data: 行双行分帧；data JSON 里
//     type=="reasoning"（data.text→reasoning_content）/ type=="text"（data 为
//     **纯字符串增量** 或 {text:…} 对象）/ type=="end"|"text_end"（完成）/
//     type=="error"（失败）。完整序列（9 帧实测）：execution/user → text_start →
//     text(×N) → text_end → aigc/end → recommend_start → recommend →
//     recommend_end → finish(空 data)。text 帧是增量直传（emitDelta）。
//  5. **reasoning_effort 映射**：low→"0"、high→"1"、max→"2"（灵犀自定义线值）；
//     不传则上游默认。实测 deepseek-v4.1-flash 与 qwen3.8_flash 加 effort="2"
//     均未出 reasoning 帧——按 magpie 同构支持该帧类型但不依赖。
//  6. **签到**：GET /api/public/v1/tasks 找 task_key=="daily_check_in"（status
//     =="claimed" 即已签）→ POST /api/public/v1/tasks/daily_check_in/claim?date=YYYY-MM-DD
//     （日期在 query、无 body）。响应 result=="ok" 为成功，
//     data.extra.today_reward 为获得的智点数。
//  7. **余额**：GET /api/public/v1/credits/balance → data.total_balance（智点）、
//     data.bonus_credits[].expire_time（到期时间，"YYYY-MM-DD" 形态）、
//     data.bonus_summary.total_value（发放总额）。
package lingxi

import (
	"time"

	"wild-work/internal/provider"
)

const Kind = provider.Lingxi

// 上游端点。均需要 Cookie wps_sid。
const (
	APIBase        = "https://lingxi.kdocs.cn"
	EpSessions     = "/api/aigc/v3/assistant/sessions"
	EpPlans        = "/api/aioffice/v1/sessions/plans"
	EpTasks        = "/api/public/v1/tasks"
	EpBalance      = "/api/public/v1/credits/balance"
	EpClaim        = "/api/public/v1/tasks/daily_check_in/claim"
	defaultBaseURL = APIBase
)

// EpCompletions 拼接对话端点（sessions/<sid>/completions）。
func EpCompletions(sessionID string) string {
	return EpSessions + "/" + sessionID + "/completions"
}

// CookieName 是灵犀网关唯一认的凭据头（与 comate 同款）。
const CookieName = "wps_sid"

// completions 专用头（实测事实 3）：三头必须齐发。
const (
	ccVersion   = "1"
	ccClientTyp = "lingxi-web"
	ccEntrance  = "default"
)

// DefaultIdleTimeout 无 config 注入时的流式空闲上限（R35：StreamHTTP 不设
// Timeout，空闲兜底由 IdleReader 承担）。
const DefaultIdleTimeout = 90 * time.Second

// FarFutureExpiry Cookie 无过期概念（失效表现为 401/UserUnauthorized，
// 需人工换 sid）。ExpiresAt 必须给远期值而**不得为 0**：0 会让
// auth.NeedsRefresh 恒真 ⇒ 每次请求都尝试刷新（不变量 21 的同款陷阱）。
func FarFutureExpiry() int64 { return time.Now().AddDate(80, 0, 0).Unix() }

// mapEffort 把 OpenAI 风格 reasoning_effort 映射成灵犀线值（实测事实 5）。
// 原样数字（"0"/"1"/"2"）直通；medium 映射到 "1"（与 high 同档，避免丢档）；
// off/none 返回空串（上游无此档，去掉字段交上游默认）。
func mapEffort(effort string) string {
	switch effort {
	case "low", "0":
		return "0"
	case "medium", "high", "1":
		return "1"
	case "max", "2":
		return "2"
	default:
		return ""
	}
}
