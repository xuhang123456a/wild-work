package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// 本测试锁一个**真实存在的接线缺口**（R129）：
//
//	降级链保存的热更新闭包原先只推内层 handler（inner.SetModelFallbacks），
//	兼容层（/v1/responses、/v1/messages）的别名表从未被推过。
//	后果：Chat 能用别名、Codex / Claude Code 不能 —— 不报错不崩溃，用户只能猜。
//
// 静态锁死两条：
//  ① 降级链 syncer 闭包里必须同时出现 inner.SetModelFallbacks 与 compat.SetAliases，
//     且别名表经 config.AliasIndex 派生（单一来源，不在装配层另写清洗逻辑）；
//  ② 兼容层构造时（启动路径）必须带上配置里的别名表，不得只靠面板热更新
//     ——否则重启后别名在 Responses/Anthropic 上静默消失。
func TestModelFallbackSyncerPushesAliasesToBothLayers(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读 main.go 失败: %v", err)
	}
	code := string(src)

	// ① 闭包体：SetModelFallbackSyncer(func(rules []config.ModelFallbackRule) { ... })
	re := regexp.MustCompile(`SetModelFallbackSyncer\(func\(rules \[\]config\.ModelFallbackRule\) \{([\s\S]*?)\n\t\}\)`)
	m := re.FindStringSubmatch(code)
	if m == nil {
		t.Fatal("未找到 SetModelFallbackSyncer 接线闭包")
	}
	body := m[1]
	if !strings.Contains(body, "inner.SetModelFallbacks(rules)") {
		t.Error("降级链 syncer 必须推内层 handler（inner.SetModelFallbacks）")
	}
	if !strings.Contains(body, "compat.SetAliases(config.AliasIndex(rules))") {
		t.Error("降级链 syncer 必须同时推兼容层别名表（compat.SetAliases(config.AliasIndex(rules))）" +
			"——漏推的表现是「Chat 能用别名、Codex 不能」")
	}

	// ② 启动路径：gateway.Router 构造里带 Aliases: config.AliasIndex(cfg.ModelFallback)
	if !strings.Contains(code, "Aliases: config.AliasIndex(cfg.ModelFallback)") {
		t.Error("兼容层启动时必须载入配置里的别名表（不得只靠面板热更新）")
	}
}

// 兼容层别名表与 compat 路由表必须是两条独立热更新通道：
// SetCompat 不得清掉别名表（否则「先存别名、后存 model_map」会让别名静默失效）。
// 这里静态确认 SetCompat 里保留了 Aliases 字段的继承（行为断言在 internal/gateway）。
func TestSetCompatKeepsAliasesField(t *testing.T) {
	src, err := os.ReadFile("../../internal/gateway/gateway.go")
	if err != nil {
		t.Fatalf("读 gateway.go 失败: %v", err)
	}
	code := string(src)
	re := regexp.MustCompile(`func \(g \*Gateway\) SetCompat\([\s\S]*?\n\}`)
	m := re.FindString(code)
	if m == "" {
		t.Fatal("未找到 SetCompat 实现")
	}
	if !strings.Contains(m, "Aliases:  g.router.Aliases") {
		t.Error("SetCompat 重建 Router 时必须继承 Aliases（否则保存 model_map 会清掉别名表）")
	}
	if !strings.Contains(code, "func (g *Gateway) SetAliases(") {
		t.Error("兼容层必须有独立的 SetAliases 热更新入口（不扩展 SetCompat 的参数）")
	}
}
