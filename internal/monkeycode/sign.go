// sign.go MonkeyCode 的 Prompt 签名（官方称 promptauth）。
//
// 算法（2026-09-23 字节级确认，见评估文档 §3.3）：
//
//	X-Ohmyagent-Signature = "v1=" + hex(HMAC-SHA256(signing_secret, system[0].text))
//
// 要点：
//   - 只签 **第一条 system 的纯文本**；后续 system 条目、messages、tools 都不参与。
//   - 编码是**小写 hex**，带 `v1=` 前缀（不是裸 hex，也不是 base64）。
//   - 签名输入**按原样字节**参与计算：官方提示词含 44 个 CRLF，任何换行归一化
//     （\r\n → \n）都会让签名对不上。本实现不对输入做任何改写。
//
// 因为本渠道固定使用 constants.signatureSystemPrompt 作为 system[0]，
// 签名只依赖 signing_secret，故按 secret 缓存——同一账号只算一次。
package monkeycode

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
)

// Sign 计算签名头取值（含 `v1=` 前缀）。
// text 必须与出站请求里 system[0].text **逐字节一致**。
func Sign(secret, text string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(text))
	return "v1=" + hex.EncodeToString(mac.Sum(nil))
}

// signatureCache 按 signing_secret 缓存签名。签名输入是常量，故缓存安全；
// 面板换账号/换 secret 时会自然出现新 key，旧条目无副作用（进程生命周期内）。
var signatureCache sync.Map // secret(string) -> signature(string)

// signatureFor 返回该 secret 对应固定 system prompt 的签名。
func signatureFor(secret string) (string, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return "", fmt.Errorf("monkeycode: 缺少 signing_secret（请在面板重新导入客户端凭据）")
	}
	if v, ok := signatureCache.Load(secret); ok {
		return v.(string), nil
	}
	sig := Sign(secret, signatureSystemPrompt)
	signatureCache.Store(secret, sig)
	return sig, nil
}
