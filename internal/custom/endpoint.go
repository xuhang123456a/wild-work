package custom

import (
	"net/url"
	"regexp"
	"strings"
)

// 接入点规范化（R119 补强）：面板里用户粘贴的往往是「文档里那条完整 curl 的地址」，
// 例如 https://x.com/v1/chat/completions，或干脆漏掉协议头的 api.x.com/v1。
// 本包内部一律按「API 根 + /chat/completions」拼请求，所以入口必须先规整，
// 否则会把尾巴带进 base，拼出 /v1/chat/completions/chat/completions 这种必死地址。

// apiTails 可安全剥离的「端点尾巴」（大小写不敏感，长的在前）。
//
// 为什么只剥这几个：自定义渠道的 Upstream 只发 Chat Completions（client.go 里写死拼
// /chat/completions），用户粘进来的地址若已含该尾巴，留着它必然是错的；
// /models 尾巴（R122）同理——面板「识别端点」让用户粘的就是目录地址，
// 留着会拼出 {root}/models/models 这种必死路径，而它看上去像「上游没有 /models」，
// 实际是我们自己拼错了。
// 其余路径段（/step_plan/v1、/api/passthrough/v1 之类）是网关自己的前缀，绝不猜测删除。
var apiTails = []string{"/chat/completions", "/completions", "/models"}

// localHostPrefixes 本机/内网地址前缀：这类地址默认 http（本地 one-api、llama.cpp、vllm）。
var localHostPrefixes = []string{"localhost", "127.", "0.0.0.0", "::1", "[::1]", "192.168.", "10.", "172.16.", "host.docker.internal"}

var schemeRE = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.\-]*://`)

// NormalizeBaseURL 把用户输入的接入点规整成 API 根。
//
// 做的五件事（无损：规整前后指向同一个端点）：
//  1. 去首尾空白与成对引号/反引号（从 markdown 或 curl -H 里复制会带上）；
//  2. 缺协议头时补一个——内网/回环补 http、其余补 https。不补的话 url.Parse 会把
//     "api.x.com/v1" 解析成 scheme="api.x.com"，报给用户的错误毫无意义；
//  3. 丢掉 ?query 与 #fragment（粘 ?key=xxx 的地址时，key 会跟着落进配置文件，
//     而本包鉴权只走 Authorization 头，留着纯泄漏）；
//  4. 剥掉结尾的 /chat/completions、/completions 尾巴（可能连粘两层，剥到不动为止）；
//  5. 去尾部斜杠（客户端拼接时统一补）。
//
// 不做的事：不猜测删除任何中间前缀段、不改 host 大小写以外的路径。
func NormalizeBaseURL(raw string) string {
	s := strings.Trim(strings.TrimSpace(raw), `"'`+"`")
	if s == "" {
		return ""
	}
	if !schemeRE.MatchString(s) {
		if strings.HasPrefix(s, "//") {
			s = "https:" + s // 协议相对写法
		} else {
			s = defaultScheme(s) + "://" + s
		}
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return strings.TrimRight(s, "/") // 已尽力，原样交给 Validate 去报错
	}
	p := strings.TrimRight(u.Path, "/")
	for {
		low, cut := strings.ToLower(p), false
		for _, t := range apiTails {
			if strings.HasSuffix(low, t) {
				p = strings.TrimRight(p[:len(p)-len(t)], "/") // 按长度切，保住原始大小写
				cut = true
				break
			}
		}
		if !cut {
			break
		}
	}
	scheme := strings.ToLower(u.Scheme)
	u.RawQuery, u.Fragment, u.RawPath = "", "", ""
	u.Path = p
	return scheme + "://" + u.Host + p
}

// defaultScheme 依 host 猜协议：内网/回环 http，其余 https。
func defaultScheme(s string) string {
	host := strings.ToLower(strings.SplitN(strings.TrimPrefix(s, "//"), "/", 2)[0])
	if i := strings.LastIndex(host, "@"); i >= 0 {
		host = host[i+1:] // 去掉 user[:pass]@ 前缀
	}
	for _, ph := range localHostPrefixes {
		if strings.HasPrefix(host, ph) {
			return "http"
		}
	}
	return "https"
}

// BaseCandidates 给出按优先级排列的候选 API 根（最多 3 个，已去重）。
//
// 「/v1 到底该不该带」是第三方网关最常见的歧义（ NormalizeBaseURL 处理不了，
// 因为带着和不带都可能是对的），只能靠嗅探试。候选顺序 = 先尊重用户填的那个。
func BaseCandidates(base string) []string {
	base = strings.TrimRight(base, "/")
	if base == "" {
		return nil
	}
	out := []string{base}
	add := func(c string) {
		// 削完不剩 host 的候选直接丢：https://v1 去掉结尾 "/v1" 会退化成 "https:"，
		// url.Parse 不报错但必定请求失败，不如不进候选列表（R121）。
		if c = strings.TrimRight(c, "/"); c == "" {
			return
		}
		if i := strings.Index(c, "://"); i < 0 || i+3 >= len(c) {
			return
		}
		for _, e := range out {
			if strings.EqualFold(e, c) {
				return
			}
		}
		out = append(out, c)
	}
	if low := strings.ToLower(base); strings.HasSuffix(low, "/v1") {
		add(base[:len(base)-3]) // 切掉结尾的 "/v1"（按长度切，保住原始大小写）
	} else {
		add(base + "/v1")
	}
	return out
}
