package custom

import (
	"testing"
)

// 接入点规范化（R121）。这里只测**静态**变换，网络侧在 discover_test.go。
func TestNormalizeBaseURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"https://api.stepfun.com/step_plan/v1", "https://api.stepfun.com/step_plan/v1"},  // 已规范，不动
		{"https://api.stepfun.com/step_plan/v1/", "https://api.stepfun.com/step_plan/v1"}, // 去尾斜杠
		// 尾巴必须剥：本包内部一律拼 {base}/chat/completions，留着尾巴等于请求必死地址
		{"https://api.stepfun.com/v1/chat/completions", "https://api.stepfun.com/v1"},
		{"https://x.com/v1/chat/completions/", "https://x.com/v1"},
		{"https://x.com/v1/completions", "https://x.com/v1"},                       // legacy 尾巴同样剥（只是不再剥第二层）
		{"https://x.com/v1/chat/completions/chat/completions", "https://x.com/v1"}, // 连粘两层要剥到不动为止
		// R122：用户常直接把目录地址（浏览器里看到的 /models）粘进来，不剥会拼出 {root}/models/models，
		// 表现为「上游没有模型目录」——其实是他自己拼错了，这个坑必须静默治好而不是报错。
		{"https://api.x.com/v1/models", "https://api.x.com/v1"},
		{"https://api.x.com/v1/models/", "https://api.x.com/v1"},
		{"https://api.x.com/models", "https://api.x.com"},
		{"https://x.com/v1/models/models", "https://x.com/v1"},
		{"https://x.com/v1/models/chat/completions", "https://x.com/v1"},                // 混合尾巴也要剥到不动
		{"https://x.com/api/passthrough/v1/models", "https://x.com/api/passthrough/v1"}, // 中间前缀段不得动
		{"https://x.com/V1/Chat/Completions", "https://x.com/V1"},                       // 大小写不敏感匹配，但保留原大小写
		// 缺协议头：内网补 http、公网补 https
		{"127.0.0.1:11434/v1", "http://127.0.0.1:11434/v1"},
		{"localhost:8000/v1", "http://localhost:8000/v1"},
		{"192.168.1.20:3000/v1", "http://192.168.1.20:3000/v1"},
		{"api.openai.com/v1", "https://api.openai.com/v1"},
		{"//api.openai.com/v1", "https://api.openai.com/v1"},
		{"user:pass@api.x.com/v1", "https://api.x.com/v1"}, // 不当内网；userinfo 不保留（鉴权只走 Bearer 头）
		// 引号/反引号是 markdown 与 curl 示例里粘出来的
		{`"https://x.com/v1"`, "https://x.com/v1"},
		{"`https://x.com/v1`", "https://x.com/v1"},
		// query/fragment 必须丢：?key=xxx 会跟着落进配置文件，而本包只走 Authorization 头
		{"https://x.com/v1?key=secret", "https://x.com/v1"},
		{"https://x.com/v1#docs", "https://x.com/v1"},
		// 中间前缀段绝不猜测删除
		{"https://x.com/api/passthrough/v1", "https://x.com/api/passthrough/v1"},
		{"https://x.com/step_plan/v1", "https://x.com/step_plan/v1"},
	}
	for _, c := range cases {
		if got := NormalizeBaseURL(c.in); got != c.want {
			t.Errorf("NormalizeBaseURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBaseCandidatesOrderAndDedupe(t *testing.T) {
	got := BaseCandidates("https://x.com/v1")
	if len(got) != 2 || got[0] != "https://x.com/v1" || got[1] != "https://x.com" {
		t.Fatalf("带 /v1 应给两个候选且原样优先：%v", got)
	}
	got = BaseCandidates("https://x.com/api/passthrough/v1")
	if len(got) != 2 || got[1] != "https://x.com/api/passthrough" {
		t.Fatalf("只该剥最后一段 /v1：%v", got)
	}
	got = BaseCandidates("https://x.com")
	if len(got) != 2 || got[0] != "https://x.com" || got[1] != "https://x.com/v1" {
		t.Fatalf("不带 /v1 应补一个候选：%v", got)
	}
	// 只有一段时不能退化成空 host
	if got = BaseCandidates("https://v1"); len(got) != 1 || got[0] != "https://v1" {
		t.Fatalf("单段地址不得被削成空：%v", got)
	}
	if got = BaseCandidates(""); len(got) != 0 {
		t.Fatalf("空输入不该给候选：%v", got)
	}
	// 尾斜杠已在入口削掉，不能产出重复项
	if got = BaseCandidates("https://x.com/v1/"); len(got) != 2 {
		t.Fatalf("尾斜杠不应多出候选：%v", got)
	}
}
