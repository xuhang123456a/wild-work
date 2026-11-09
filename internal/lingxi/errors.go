package lingxi

import (
	"fmt"
	"strings"

	"wild-work/internal/provider"
)

// hardCreditMarkers 智点（积分）耗尽文案。灵犀侧重叠 comate 的 WPS 会员体系
// 文案；14018 是 WPS 网关把积分耗尽写成数字 code 的形态（comate 侧实测，
// 灵犀同一账号体系）。
var hardCreditMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required",
	"credit not enough", "not enough credit",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
	"14018", "智点不足", "智点余额不足",
}

// sessionDeadMarkers 会话失效文案：UserUnauthorized 是灵犀 public/v1 系列的
// 权威形态（实测）；not_login 是 WPS 网关通用形态。
var sessionDeadMarkers = []string{
	"UserUnauthorized", "not_login", "session expired", "not logged in",
	"登录失效", "凭据失效", "请重新登录",
}

// modelUnavailableMarkers 模型不可用文案（comate 侧实测同体系文案，
// 灵犀模型目录是动态的，失效模型可能回同款提示）。
var modelUnavailableMarkers = []string{
	"指定模型暂不可用", "model not available", "model_unavailable",
}

// IsHardCreditBody 报告响应体是否命中智点耗尽文案。
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

// Classify 实现 provider.Upstream。顺序敏感：会话失效 → 智点耗尽 → 模型不可用
// → 状态码（与 comate 同构；灵犀业务错误也可能以 200+文案呈现，文案判定在前）。
func Classify(status int, body string) provider.ErrKind {
	if IsSessionDeadBody(body) {
		return provider.ErrSessionDead
	}
	if IsHardCreditBody(body) {
		return provider.ErrHardCredit
	}
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

// StreamError 承载「SSE 流内 error 帧」或「200+业务错误信封」的失败。
// 实现 provider.StreamErrorClassifier。
type StreamError struct {
	Model string // 出错的模型 ID（用于记模型级冷却）
	Code  string // 上游 code（有则填）
	Msg   string // 上游原文
	kind  provider.ErrKind
}

func (e *StreamError) Error() string {
	return fmt.Sprintf("lingxi stream error model=%s code=%s: %s", e.Model, e.Code, e.Msg)
}

// Kind 返回账号侧处置口径。kind 未归类时走 ErrPassthrough（原文透传，
// 不罚账号——流内错误的性质只有已归类的那几种才可安全处置）。
func (e *StreamError) Kind() provider.ErrKind {
	if e.kind != provider.ErrNone {
		return e.kind
	}
	return provider.ErrPassthrough
}

// newStreamError 按错误文案构造 StreamError：命中 markers 即填 kind，
// 让 handler 能换号/罚号/标记失效（对齐 comate 的 parseErrFrame）。
// 不填 kind 的后果（2026-10-29 审查 W1）：Kind() 恒 ErrPassthrough，
// 流内「请重新登录/智点不足」帧永远不触发 MarkSessionDead/冷却，失效号反复中招。
func newStreamError(model, msg string) *StreamError {
	se := &StreamError{Model: model, Msg: msg}
	switch {
	case IsSessionDeadBody(msg):
		se.kind = provider.ErrSessionDead
	case IsHardCreditBody(msg):
		se.kind = provider.ErrHardCredit
	case IsModelUnavailableBody(msg):
		se.kind = provider.ErrNotFound
	}
	return se
}
