package traework

import (
	"errors"
	"strings"
	"testing"
)

// 回归（issue #50）：TraeWork 登录 400/10101 的误导根因修复。
// 换 origin 重试曾把首因（403/20401 设备数上限）覆盖成语义更模糊的兜底错误
// （400/10101 无效参数）。现在：
//  1. 4xx 属终态：立即返回携带**首个** origin 真实响应的 *AuthCodeRejectedError，
//     不再换回退 origin 掩盖首因；
//  2. 单表识别 20401（设备数上限）→ 可行动提示。
func TestIsDeviceLimitReached(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"普通 4xx 10101", &AuthCodeRejectedError{Status: 400, Origin: "https://api.trae.com.cn", Body: `{"Error":{"Code":"10101","Message":"无效参数"}}`}, false},
		{"20401 设备数上限", &AuthCodeRejectedError{Status: 403, Origin: "https://api.trae.cn", Body: `{"ResponseMetadata":{"Error":{"Code":"20401","Message":"Device limit reached.","StandardCode":"040034"}}}`}, true},
		{"20401 但非 403", &AuthCodeRejectedError{Status: 400, Origin: "x", Body: `"Code":"20401"`}, false},
		{"device limit 文案(小写)", &AuthCodeRejectedError{Status: 403, Origin: "x", Body: `{"Error":{"Message":"device limit reached"}}`}, true},
		{"包裹在普通 error 里", errors.New("AuthCode ExchangeToken failed: ..."), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsDeviceLimitReached(c.err); got != c.want {
				t.Errorf("IsDeviceLimitReached(%v)=%v want %v", c.err, got, c.want)
			}
		})
	}
}

// AuthCodeRejectedError 必须透出首因原文（origin + status + body），不能只剩模糊文案。
func TestAuthCodeRejectedErrorCarriesFirstOrigin(t *testing.T) {
	e := &AuthCodeRejectedError{Status: 403, Origin: "https://api.trae.cn", Body: `{"Error":{"Code":"20401"}}`}
	msg := e.Error()
	if !strings.Contains(msg, "api.trae.cn") || !strings.Contains(msg, "403") || !strings.Contains(msg, "20401") {
		t.Errorf("Error() 应同时透出 origin/status/body 三个维度，got: %s", msg)
	}
}