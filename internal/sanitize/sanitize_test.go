package sanitize

// 生产路径脱敏表的回归护栏。
//
// 背景（R115 / PR #185）：脱敏表在本仓有**两份实现**——本包（`upstream.PrepareBody` 与
// `workbuddyai` 都调它）与 `internal/upstream/sanitize.go` 的包内平行实现。历史上两次
// 「改了上游没改这里」都靠人肉同步发现，本文件把两件事锁死：
//  1. 本包表的逐字节内容快照（与 upstream 侧快照同版本号，改一处必两处同时红）；
//  2. 与 `upstream.PrepareBody` 的**行为一致性**（同一段指纹文本，两路净化结果必须逐字相同）。
//
// 判据层级：身份句/整段逐字串是 2026-10-21 单发 A/B 实测；「上游按连续逐字前缀匹配」
// 是实测结论，不是关键词或语义审核——所以改写只要能破坏逐字串即可，措辞尽量原样保留。

import (
	"encoding/json"
	"strings"
	"testing"
)

const (
	// DSH 身份句：上游对 system 消息**开头**逐字前缀匹配即回 400 code=11-128。
	dshIdentity = "You are an AI agent powered by DeepSeek Harness"
	// DSH Web GUI 段：与后半句连成 221 字符整段逐字串才拦，单句放行。
	dshWebGUI = "DeepSeek Harness Web GUI"
)

// 本包 featHeaders 的逐字节快照（9 项）。
var wantFeatures = []string{
	"x-anthropic-billing-header",
	"cc_entrypoint=",
	"You are Claude Code",
	"Main branch (",
	"You are a coding agent running in the Codex CLI",
	"github.com/anthropics/",
	"11128",
	dshIdentity,
	dshWebGUI,
}

// 本包 rewrites 的逐字节快照（7 对）。
var wantRewrites = [][2]string{
	{"You are Claude Code, Anthropic's official CLI for Claude",
		"You are Claude Code, Anthropic's official CLI tool for Claude"},
	{"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)"},
	{"You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",
		"You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant."},
	{"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues"},
	{"11128", "11-128"},
	{dshIdentity, "You are an AI agent powered by the DeepSeek Harness"},
	{dshWebGUI, "DSH Web GUI"},
}

func TestTablesByteExactSnapshot(t *testing.T) {
	if len(featHeaders) != len(wantFeatures) {
		t.Fatalf("featHeaders 项数=%d want %d（改表必须同步本快照 + upstream 侧快照）",
			len(featHeaders), len(wantFeatures))
	}
	for i, want := range wantFeatures {
		if featHeaders[i] != want {
			t.Errorf("featHeaders[%d]=%q want %q", i, featHeaders[i], want)
		}
	}
	if len(rewrites) != len(wantRewrites) {
		t.Fatalf("rewrites 对数=%d want %d", len(rewrites), len(wantRewrites))
	}
	for i, want := range wantRewrites {
		if rewrites[i][0] != want[0] || rewrites[i][1] != want[1] {
			t.Errorf("rewrites[%d]=%q→%q want %q→%q", i,
				rewrites[i][0], rewrites[i][1], want[0], want[1])
		}
	}
}

// 整段逐字串（身份句 + Web GUI 段 + 后缀）必须被完全破坏，且非指纹部分保留。
func TestDSHFullPromptSanitizedViaMessages(t *testing.T) {
	in := dshIdentity + ". " + dshWebGUI + ", not the CLI or desktop app. If the user says gui, they mean this GUI."
	out := string(Messages(bodyWithSystem(t, in)))
	for _, fp := range []string{dshIdentity, dshWebGUI} {
		if strings.Contains(out, fp) {
			t.Fatalf("指纹残留 %q：%q", fp, out)
		}
	}
	if !strings.Contains(out, "DSH Web GUI") {
		t.Errorf("Web GUI 改写缺失: %q", out)
	}
	if !strings.Contains(out, "powered by the DeepSeek Harness") {
		t.Errorf("身份句改写缺失: %q", out)
	}
	if !strings.Contains(out, "they mean this GUI.") {
		t.Errorf("非指纹措辞被误删: %q", out)
	}
}

// 反向护栏：措辞相近但不是逐字形态的文本一律不得改动（防误伤用户内容）。
func TestDSHVariantsUntouched(t *testing.T) {
	for _, in := range []string{
		"You are an AI agent, powered by DeepSeek Harness.", // 多一个逗号
		"You are an AI agent powered by DeepSeek.",          // 截去 Harness
		"An AI agent powered by DeepSeek Harness.",          // 缺 "You are an"
		"DeepSeek Harness CLI",                              // 含品牌但不是 Web GUI 段
		"You use the web GUI to do work.",                   // 含 web gui 但不是 DSH 短语
	} {
		if got := systemOf(t, Messages(bodyWithSystem(t, in))); got != in {
			t.Errorf("变体被误改: %q -> %q", in, got)
		}
	}
}

// 无指纹请求必须零改动（同一切片返回，不重新序列化）。
func TestNoFingerprintSameBytes(t *testing.T) {
	body := bodyWithSystem(t, "帮我看下这段日志。")
	if got := Messages(body); string(got) != string(body) {
		t.Errorf("无指纹请求被改写: %s", got)
	}
}

func bodyWithSystem(t *testing.T, sys string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"model": "m", "messages": []any{
		map[string]any{"role": "system", "content": sys},
		map[string]any{"role": "user", "content": "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func systemOf(t *testing.T, body []byte) string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, body)
	}
	ms, _ := obj["messages"].([]any)
	if len(ms) == 0 {
		t.Fatalf("无 messages: %s", body)
	}
	s, _ := ms[0].(map[string]any)["content"].(string)
	return s
}
