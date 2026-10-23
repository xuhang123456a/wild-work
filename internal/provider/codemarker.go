package provider

import "strings"

// CodeMarker 判定（已小写化的）body 里是否出现业务码 code（如 11115 / 11101 / 11135 / 14018）。
//
// 各渠道 Classify 共用此判定（含 internal/upstream），避免两套口径：
//   - 字面量 `"code":11135` 只覆盖紧凑 JSON，上游一旦美化输出（`{"code": 11135}`）
//     或用字符串码（`"code":"11135"`）就漏判——漏判的后果不是「少一条日志」，
//     是把请求级错误误归 ErrClient 并罚健康账号。
//   - 反过来，裸 `Contains(lower, "11115")` 太松：request_id / token 计数等任意文本
//     含这五个数字都会误命中，把该罚号的账号错误透传出去。
//
// 因此只认 `"code":` / `'code':` 键后的值；值前允许空白与引号；
// 码后紧跟字母/数字不算命中（`"code":111350` 不该命中 11135）。
//
// lower 必须是 strings.ToLower(body)（键名大小写不敏感场景已由调用方归一）。
func CodeMarker(lower, code string) bool {
	for _, key := range []string{`"code":`, `'code':`} {
		for off := 0; ; {
			i := strings.Index(lower[off:], key)
			if i < 0 {
				break
			}
			rest := strings.TrimLeft(lower[off+i+len(key):], ` "'`)
			off += i + len(key)
			if !strings.HasPrefix(rest, code) {
				continue
			}
			if tail := rest[len(code):]; tail != "" && isASCIIAlnum(tail[0]) {
				continue
			}
			return true
		}
	}
	return false
}

func isASCIIAlnum(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
