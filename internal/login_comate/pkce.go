package logincomate

// pkce.go —— 扫码登录的两个随机参数：CSRF 串与 PKCE verifier/challenge。
//
// 为什么要跟登录页逐参数对齐（R137）：
//   - CSRF：WPS 账号域对所有写请求做「双提交」校验，缺了直接
//     403 `{"result":"csrfRequire","msg":"非法访问（错误码：0x00018）"}`；
//   - PKCE：非 WPS 客户端分支下，登录页先 `ksoAccount.generatePKCEParam()` 生成
//     verifier/challenge，再把 **challenge** 作为 `code_challenge` 参数传给
//     `GET /api/v3/login_qrcode`，最后在 `grant_token` / `loginCallbackApp` 里带 **verifier**。
//     服务端不申请 challenge 的话，上游并未为这个 loginid 绑定 verifier，
//     带 verifier 的换票分支自然对不上。

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

// newCSRFToken 生成 32 位 CSRF 串（字符表与登录页 common.createCsrftoken 一致）。
func newCSRFToken() string {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	out := make([]byte, 32)
	for i, b := range buf {
		out[i] = csrfAlphabet[int(b)%len(csrfAlphabet)]
	}
	return string(out)
}

// newPKCEPair 生成 PKCE code_verifier / code_challenge（S256，RFC 7636）：
// verifier = base64url(32 字节随机)，challenge = base64url(sha256(verifier))，均无填充。
func newPKCEPair() (verifier, challenge string) {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	return verifier, challengeOf(verifier)
}

// challengeOf 由 verifier 反推 challenge（二维码过期换新时要重新上报同一个 challenge）。
func challengeOf(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
