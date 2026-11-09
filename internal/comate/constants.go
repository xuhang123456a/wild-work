// Package comate 是 WPS Comate（comate.wps.cn）渠道的上游实现。
//
// 协议来源：本项目逆向参考了 [DiEr-TouShou/dsh-connect-comate](https://github.com/DiEr-TouShou/dsh-connect-comate)
// （MIT）—— 一个把本机已登录的 WPS Comate 账号接入 DeepSeek Harness 的插件。
// 其对 comate 网关的鉴权形态（Cookie-only）、思考档位（thinking_levels 线值）、
// 错误文案分类（HARD_CREDIT_MARKERS / SESSION_DEAD_MARKERS）与请求体规范化
// （developer→system、tool_choice 展平）的知识为本渠道的逆向依据；
// 本项目只取协议知识，其 DSH 插件封装、凭据密封存储、loopback shim 未采用。
//
// 凭据：WPS 账号的 **wps_sid Cookie**（www.wps.cn 登录后浏览器 Cookie 里的 wps_sid），
// 由面板「粘贴 wps_sid」写入 auths/（见 app.SubmitComateSID）。不走 OAuth ——
// Comate 网关只认 Cookie，Authorization Bearer 实测 401（apiKey、sid 均无效），
// 且 Cookie 不轮换 ⇒ 本渠道没有 RefreshToken，也没有保活/签到需求。
//
// 渠道定位：粘贴型多账号渠道（一个 WPS 账号一份 wps_sid），与 cline 同形态。
// 模型面**不做任何本地假设**：绑死上游 /models 的 items 数组原样透传
// （不改名、不过滤、无静态表）。
//
// 必须知道的实测事实（2026-10-27/28，逐项对照实验，见 docs/comate渠道接入备忘.md）：
//  1. **鉴权只认 Cookie**：`Cookie: wps_sid=<sid>` → 200；Authorization Bearer
//     （apiKey 或 sid）→ 401 `not_login`；x-api-key / api-key / x-wps-sid 头、
//     query 参数均 401。Cookie 在场时**任意 bogus Authorization 头不影响**（200）。
//  2. **业务错误压平成 HTTP 200**：无效模型返回 200 +
//     `{"code":"MULTIMODAL_TOKEN_QUOTA_EXCEEDED","msg":"指定模型暂不可用…"}`（非流式）
//     或 200 + 一帧 **mock SSE**（`id:"chatcmpl-mock-…"`、`model:"mock"`，错误文案
//     放在 delta.content、finish_reason=stop）—— 伪装成正常回答，必须识别并转成
//     错误，否则客户端拿到「模型回了一句报错」的假成功。
//  3. **响应 model 字段是短名**：请求 `673940726/zhipu/glm-5.3-flash//public`，
//     响应 `model:"glm-5.3-flash"`。聚合时回填客户端请求的原始模型名（R14 同款）。
//  4. **reasoning_effort 直通**：`reasoning_effort:"off"` 实测生效（reasoning_content
//     0 字符）；"none" 被接受但**静默忽略**（思考量不变）；low/medium/high 原样生效。
//     reasoning_effort:false 会被网关 400。
//  5. **usage 带 credits**：`{"prompt_tokens":…,"completion_tokens":…,"credits":0.01}`，
//     credits 是本次消耗的积分（consume_ratio × tokens），供 ledger 记账。
package comate

import (
	"time"

	"wild-work/internal/provider"
)

const Kind = provider.Comate

// 上游端点。/models 与 /chat/completions 都需要 Cookie wps_sid。
const (
	APIBase        = "https://comate.wps.cn/llmproxy/v1/user"
	EpChat         = "/chat/completions"
	EpModels       = "/models"
	defaultBaseURL = APIBase
)

// CookieName 是 WPS 网关唯一认的凭据头。
const CookieName = "wps_sid"

// max_tokens 实测（2026-10-28）：8 → 正常 200（不像 cline 会 400）；
// 上游 /models 的 max_tokens.max = 32768 是权威上限，超出按上游报错透传。
// 客户端不传时**一律不传**（实测不传最稳）。
const (
	minUpstreamMaxTokens = 1
	maxUpstreamMaxTokens = 32768
)

// DefaultIdleTimeout 无 config 注入时的流式空闲上限（R35：StreamHTTP 不设 Timeout，
// 空闲兜底由 IdleReader 承担）。
const DefaultIdleTimeout = 90 * time.Second

// FarFutureExpiry Cookie 无过期概念（失效表现为 401 not_login，需人工换 sid）。
// ExpiresAt 必须给远期值而**不得为 0**：0 会让 auth.NeedsRefresh 恒真
// ⇒ 每次请求都尝试刷新（不变量 21 的同款陷阱）。
func FarFutureExpiry() int64 { return time.Now().AddDate(80, 0, 0).Unix() }
