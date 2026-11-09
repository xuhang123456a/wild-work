// 降级链别名保存校验矩阵（R129）。
//
// 后端是唯一真相：即使绕过前端直接调 SetModelFallback 也拦得住。本文件只断言
// 外部可观察行为——「保存调用返回什么错误文案」「配置重新读出来是什么」。
package app

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/config"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
	"wild-work/internal/server"
)

// noopUpstream 只满足接口，CachedChannelModels 不会真正调它（静态表优先）。
type noopUpstream struct{}

func (noopUpstream) RefreshToken(*auth.Auth) error                                 { return nil }
func (noopUpstream) FetchModels(*auth.Auth) ([]provider.ModelInfo, error)          { return nil, nil }
func (noopUpstream) FetchModelPricing(*auth.Auth) ([]provider.ModelPricing, error) { return nil, nil }
func (noopUpstream) UserResource(*auth.Auth) (int64, error)                        { return 0, nil }
func (noopUpstream) UserResourceDetail(*auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}
func (noopUpstream) DailyCheckin(*auth.Auth) error         { return nil }
func (noopUpstream) Classify(int, string) provider.ErrKind { return provider.ErrServer }
func (noopUpstream) ChatStream(*auth.Auth, []byte) (io.ReadCloser, int, []byte, error) {
	return nil, 0, nil, nil
}
func (noopUpstream) Stream(http.ResponseWriter, io.Reader, string) (map[string]any, error) {
	return nil, nil
}
func (noopUpstream) Aggregate(io.Reader, string) (map[string]any, error) { return nil, nil }

// newAliasApp 装配一个可保存降级链的 App（无账号运行时 ⇒ 模型列表为空）。
func newAliasFallbackApp(t *testing.T, modelMap map[string]string) (*App, string) {
	t.Helper()
	tmpDir := appTempDir(t)
	cfg := config.Default()
	cfg.StateFile = tmpDir + "/state.json"
	cfgPath := tmpDir + "/config.json"
	cfg.Compat.ModelMap = modelMap
	if err := config.Save(cfg, cfgPath); err != nil {
		t.Fatal(err)
	}
	a, err := New(Options{ConfigPath: cfgPath, Config: cfg, Runtimes: map[provider.Kind]*Runtime{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(a.Close)
	return a, cfgPath
}

// rule 构造一条最小合法规则（主模型 oczen/big-pickle，备胎 workbuddy/glm-free）。
func rule(name string) config.ModelFallbackRule {
	return config.ModelFallbackRule{From: "oczen/big-pickle", To: []string{"workbuddy/glm-free"}, Name: name}
}

// TestAliasValidationRejects 六条硬拒各一例：错误文案必须点名是哪条规则的哪个名字。
func TestAliasValidationRejects(t *testing.T) {
	cases := []struct {
		label  string
		rules  []config.ModelFallbackRule
		substr string
	}{
		{
			"含斜杠",
			[]config.ModelFallbackRule{rule("workbuddy/fast")},
			"不能含 /",
		},
		{
			"含空白",
			[]config.ModelFallbackRule{rule("fa st")},
			"不能含空白、逗号或 *",
		},
		{
			"含逗号",
			[]config.ModelFallbackRule{rule("fa,st")},
			"不能含空白、逗号或 *",
		},
		{
			"含星号",
			[]config.ModelFallbackRule{rule("fa*")},
			"不能含空白、逗号或 *",
		},
		{
			"与其他规则重名",
			[]config.ModelFallbackRule{
				rule("fast"),
				{From: "workbuddy/glm-5.2", To: []string{"oczen/big-pickle"}, Name: "fast"},
			},
			"重名",
		},
		{
			"与 model_map 键重名",
			[]config.ModelFallbackRule{rule("gpt-5")},
			"compat.model_map 的键重名",
		},
		{
			"与 model_map 通配键重名",
			[]config.ModelFallbackRule{rule("claude-*")},
			"不能含空白、逗号或 *", // `*` 先被形态规则拦下（通配键同样不合法）
		},
		{
			"与渠道名重名",
			[]config.ModelFallbackRule{rule("workbuddy")},
			"与渠道名重名",
		},
		{
			"备胎里写别名",
			[]config.ModelFallbackRule{
				{From: "oczen/big-pickle", To: []string{"fast"}, Name: ""},
				{From: "workbuddy/glm-free", To: []string{"oczen/big-pickle"}, Name: "fast"},
			},
			"备胎链只能写 channel/model 全路径",
		},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			a, cfgPath := newAliasFallbackApp(t, map[string]string{"gpt-5": "workbuddy/gpt-5"})
			_, err := a.SetModelFallback(c.rules)
			if err == nil {
				t.Fatalf("应被硬拒，实际保存成功")
			}
			if !strings.Contains(err.Error(), c.substr) {
				t.Fatalf("错误文案应含 %q，实际 %q", c.substr, err.Error())
			}
			// 被拒的配置不得落盘
			saved, _ := config.Load(cfgPath)
			if len(saved.ModelFallback) != 0 {
				t.Fatalf("被拒的规则不得落盘，实际 %+v", saved.ModelFallback)
			}
		})
	}
}

// TestAliasEmptyNameIsValid 空 Name = 无别名，合法且不报错。
func TestAliasEmptyNameIsValid(t *testing.T) {
	a, cfgPath := newAliasFallbackApp(t, nil)
	warns, err := a.SetModelFallback([]config.ModelFallbackRule{rule("")})
	if err != nil {
		t.Fatalf("空别名应合法，实际 %v", err)
	}
	if len(warns) != 0 {
		t.Fatalf("空别名不应产生警告，实际 %v", warns)
	}
	saved, _ := config.Load(cfgPath)
	if len(saved.ModelFallback) != 1 || saved.ModelFallback[0].Name != "" {
		t.Fatalf("空别名应原样落盘，实际 %+v", saved.ModelFallback)
	}
}

// TestAliasValidNameSaved 合法别名保存后能原样读回（配置可备份/迁移/手工编辑）。
func TestAliasValidNameSaved(t *testing.T) {
	a, cfgPath := newAliasFallbackApp(t, nil)
	if _, err := a.SetModelFallback([]config.ModelFallbackRule{rule("fast")}); err != nil {
		t.Fatalf("合法别名应保存成功，实际 %v", err)
	}
	saved, _ := config.Load(cfgPath)
	if len(saved.ModelFallback) != 1 || saved.ModelFallback[0].Name != "fast" {
		t.Fatalf("别名应落盘为 fast，实际 %+v", saved.ModelFallback)
	}
}

// TestAliasBareNameOverlapIsWarning 别名与当前模型列表的某个裸名重合 ⇒ 软警告，
// **允许保存**；模型列表为空（上游不可达）时静默跳过、不阻塞保存。
func TestAliasBareNameOverlapIsWarning(t *testing.T) {
	a, cfgPath := newAliasFallbackApp(t, nil)
	// 无 handler ⇒ 模型列表不可得 ⇒ 静默跳过（不阻塞）
	warns, err := a.SetModelFallback([]config.ModelFallbackRule{rule("glm-free")})
	if err != nil || len(warns) != 0 {
		t.Fatalf("模型列表不可得时应静默跳过软警告，实际 warns=%v err=%v", warns, err)
	}
	saved, _ := config.Load(cfgPath)
	if len(saved.ModelFallback) != 1 {
		t.Fatalf("软警告场景必须允许保存，实际 %+v", saved.ModelFallback)
	}

	// 有 handler 且模型列表含裸名 glm-free ⇒ 软警告（仍然允许保存）
	a2, cfgPath2 := newAliasFallbackApp(t, nil)
	a2.SetHandler(newModelListHandler(t, "glm-free"))
	warns2, err := a2.SetModelFallback([]config.ModelFallbackRule{rule("glm-free")})
	if err != nil {
		t.Fatalf("裸名重合是软警告，不得硬拒：%v", err)
	}
	if len(warns2) != 1 || !strings.Contains(warns2[0], "裸名重合") {
		t.Fatalf("应产生一条裸名重合软警告，实际 %v", warns2)
	}
	saved2, _ := config.Load(cfgPath2)
	if len(saved2.ModelFallback) != 1 || saved2.ModelFallback[0].Name != "glm-free" {
		t.Fatalf("软警告不得阻止落盘，实际 %+v", saved2.ModelFallback)
	}
}

// newModelListHandler 造一个只有静态模型表（无上游请求）的 server.Handler，
// 让 CachedChannelModels 能返回给定的模型 ID（软警告的事实来源）。
func newModelListHandler(t *testing.T, ids ...string) *server.Handler {
	t.Helper()
	p := pool.New(t.TempDir() + "/state-ml.json")
	p.Add(&auth.Auth{UID: "ml-uid"})
	infos := make([]provider.ModelInfo, 0, len(ids))
	for _, id := range ids {
		infos = append(infos, provider.ModelInfo{ID: id})
	}
	return server.NewHandler(server.Config{
		Runtimes: map[provider.Kind]*server.Runtime{
			provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: p, Upstream: &noopUpstream{}, StaticModels: infos},
		},
	})
}

// TestAliasDirtyNameDerivedSkipped 手改 config.json 造出的脏别名不会进索引
// （派生时兜底跳过；保存路径与运行时同源）。
func TestAliasDirtyNameDerivedSkipped(t *testing.T) {
	idx := config.AliasIndex([]config.ModelFallbackRule{
		{From: "oczen/a", To: []string{"workbuddy/b"}, Name: "ok"},
		{From: "oczen/c", To: []string{"workbuddy/d"}, Name: "bad/name"},
		{From: "oczen/e", To: []string{"workbuddy/f"}, Name: ""},
		{From: "oczen/g", To: []string{"workbuddy/h"}, Name: "ok"}, // 重复：先到先得
	})
	if len(idx) != 1 || idx["ok"] != "oczen/a" {
		t.Fatalf("脏名/空名/重复名应被跳过，实际 %+v", idx)
	}
}
