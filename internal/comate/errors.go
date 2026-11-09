package comate

import (
	"fmt"
	"strings"

	"wild-work/internal/provider"
)

// hardCreditMarkers 积分耗尽文案（中英文都认；上游 code=14018 的数字形态也在内）。
// 实测/来源：WorkBuddy 部署记录（14018 Credits exhausted）+ DSH 插件
// dsh-connect-comate（https://github.com/DiEr-TouShou/dsh-connect-comate，MIT）的
// HARD_CREDIT_MARKERS。
var hardCreditMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required",
	"credit not enough", "not enough credit",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
	"14018",
}

// sessionDeadMarkers 会话失效文案：401 not_login 是权威形态；
// 12153 是网关把会话失效写成数字 code 的形态（DSH 插件实测）。
var sessionDeadMarkers = []string{
	"not_login", "12153", "session expired", "not logged in",
	"Offline user session not found",
}

// modelUnavailableMarkers 模型不可用文案：实测无效模型返回 HTTP 200 +
// `{"code":"MULTIMODAL_TOKEN_QUOTA_EXCEEDED","msg":"指定模型暂不可用，请切换其他模型后重试"}`。
// 这是**模型级**属性（换账号必复现）⇒ ErrNotFound（短冷却，不罚整号）。
var modelUnavailableMarkers = []string{
	"指定模型暂不可用", "model not available", "model_unavailable",
}

// IsHardCreditBody 报告响应体是否命中积分耗尽文案。
func IsHardCreditBody(body string) bool {
	lower := strings.ToLower(body)
	for _, m := range hardCreditMarkers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// IsSessionDeadBody 报告响应体是否命中会话失效文案。
func IsSessionDeadBody(body string) bool {
	for _, m := range sessionDeadMarkers {
		if strings.Contains(body, m) {
			return true
		}
	}
	return false
}

// IsModelUnavailableBody 报告响应体是否命中「模型暂不可用」文案。
func IsModelUnavailableBody(body string) bool {
	for _, m := range modelUnavailableMarkers {
		if strings.Contains(body, m) {
			return true
		}
	}
	return false
}

// Classify 实现 provider.Upstream。顺序敏感：会话失效 → 积分耗尽 → 模型不可用 → 状态码。
//
// ⚠️ 本渠道业务错误常以 **HTTP 200** 呈现（见 constants.go 实测事实 2），
// 所以文案判定必须在状态码分支**之前**——200 + 错误文案的响应会先在这里被截住。
func Classify(status int, body string) provider.ErrKind {
	// 1) 会话失效：sid 过期/被踢。多账号池里禁用该号是正确行为。
	//    Cookie 不可刷新 ⇒ 不变量 19 的自愈在此不适用，只能人工换 sid。
	if IsSessionDeadBody(body) {
		return provider.ErrSessionDead
	}
	// 2) 积分耗尽：账号级硬冷却（12h 截到发奖窗口）。
	if IsHardCreditBody(body) {
		return provider.ErrHardCredit
	}
	// 3) 模型暂不可用：模型级属性，换账号必复现 ⇒ 短冷却，不罚整号。
	if IsModelUnavailableBody(body) {
		return provider.ErrNotFound
	}

	switch {
	case status == 401 || status == 403:
		return provider.ErrSessionDead
	case status == 429:
		return provider.ErrSoftRate
	case status == 402:
		return provider.ErrHardCredit
	case status == 404:
		return provider.ErrNotFound
	case status >= 500:
		return provider.ErrServer
	case status >= 400:
		return provider.ErrClient
	default:
		return provider.ErrClient
	}
}

// StreamError 承载「HTTP 200 + mock SSE 帧」的失败（见 constants.go 实测事实 2）。
// 实现 provider.StreamErrorClassifier。
type StreamError struct {
	Model string // 出错的模型 ID（用于记模型级冷却）
	Code  string // 上游 code（MULTIMODAL_TOKEN_QUOTA_EXCEEDED 等）
	Msg   string // 上游原文（msg 字段或 mock 帧的 content）
	kind  provider.ErrKind
}

func (e *StreamError) Error() string {
	return fmt.Sprintf("comate stream error model=%s code=%s: %s", e.Model, e.Code, e.Msg)
}

// Kind 返回账号侧处置口径：mock 帧几乎都源于上游模型/池状态，与账号健康无关，
// 误罚整号会把好号轮空（R16/R38 同款教训）。
func (e *StreamError) Kind() provider.ErrKind {
	if e.kind != provider.ErrNone {
		return e.kind
	}
	return provider.ErrPassthrough
}
