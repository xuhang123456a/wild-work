package app

import (
	"os"
	"path/filepath"
	"testing"

	"wild-work/internal/config"
	"wild-work/internal/custom"
	"wild-work/internal/provider"
)

// TestCustomChannelRenameCascade 覆盖 R122 第 1 条：渠道标识**就是** provider.Kind（state 文件、
// 虚拟账号 UID、模型前缀全拼它），放开改名后必须同时做到：
// ① 面板把新名+脱敏串提交回来时，真实密钥要按**旧名**找回（否则被空串覆盖并落盘）；
// ② 以 `旧标识/` 为键或值前缀的四类引用配置联动改写（它们只校形状不校渠道存在性，
//
//	不改写就是「配置还在、永久静默失效」）；
//
// ③ 旧 state 文件留 .old 不硬删（照 migrateStateFiles 先例，可回滚）。
func TestCustomChannelRenameCascade(t *testing.T) {
	tmpDir := appTempDir(t)
	cfg := config.Default()
	cfg.StateFile = tmpDir + "/state.json"
	cfgPath := tmpDir + "/config.json"

	cfg.CustomChannels = []custom.ChannelConfig{
		{Name: "stepfun", BaseURL: "https://api.stepfun.com/v1", APIKey: "sk-real-secret-1234"},
	}
	cfg.Compat.DefaultChannel = "stepfun"
	cfg.Compat.ModelMap = map[string]string{
		"glm-4":     "stepfun/glm-4",
		"other":     "workbuddy/other", // 别的渠道引用不得被牵连
		"keep.bare": "stepfun",         // 无 / 前缀形态：DefaultChannel 之外的裸值不参与改写
	}
	cfg.ModelFallback = []config.ModelFallbackRule{
		{From: "stepfun/step-5", To: []string{"stepfun/glm-4", "workbuddy/glm-5.2"}},
	}
	cfg.ReasoningEfforts = map[string]string{
		"stepfun/step-5": "high",
		"workbuddy/x":    "low",
		"*":              "medium",
	}
	_ = config.Save(cfg, cfgPath)

	// 旧 state 文件（模拟跑过之后留下的池子快照）
	oldState := filepath.Join(tmpDir, "state-custom-stepfun.json")
	if err := os.WriteFile(oldState, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var pushed int // fbSyncer 必须被推一次，否则 server.Handler 里的内存副本仍是旧 key
	var fbPushed int
	a, err := New(Options{ConfigPath: cfgPath, Config: cfg, Runtimes: map[provider.Kind]*Runtime{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	a.SetCustomChannelsSyncer(func([]custom.ChannelConfig) { pushed++ })
	// fbSyncer/efSyncer 由 main.go 装配，测试里直接覆盖同名字段
	a.fbSyncer = func([]config.ModelFallbackRule) { fbPushed++ }
	a.efSyncer = func(map[string]string) {}

	// 面板路径：先读脱敏回显，再把「新标识 + 脱敏 key」提交回来
	shown := a.GetState().CustomChannels
	if !isMaskedKey(shown[0].APIKey) {
		t.Fatalf("回显应为脱敏形态：%q", shown[0].APIKey)
	}
	renamed := []custom.ChannelConfig{{
		Name: "stepfun2", BaseURL: shown[0].BaseURL, APIKey: shown[0].APIKey,
		Models: shown[0].Models,
	}}
	if err := a.SetCustomChannels(renamed, customRename{New: "stepfun2", Old: "stepfun"}); err != nil {
		t.Fatalf("改名提交失败: %v", err)
	}

	// ① 真实密钥必须原样活着（按旧名找回）
	saved, _ := config.Load(cfgPath)
	if len(saved.CustomChannels) != 1 || saved.CustomChannels[0].Name != "stepfun2" {
		t.Fatalf("渠道未按新名落盘：%+v", saved.CustomChannels)
	}
	if got := saved.CustomChannels[0].APIKey; got != "sk-real-secret-1234" {
		t.Fatalf("改名后真实密钥被洗掉了：%q", got)
	}

	// ② 四类引用联动
	if saved.Compat.DefaultChannel != "stepfun2" {
		t.Errorf("default_channel 未联动：%q", saved.Compat.DefaultChannel)
	}
	if saved.Compat.ModelMap["glm-4"] != "stepfun2/glm-4" {
		t.Errorf("model_map 未联动：%q", saved.Compat.ModelMap["glm-4"])
	}
	if saved.Compat.ModelMap["other"] != "workbuddy/other" {
		t.Errorf("无关引用被误改：%q", saved.Compat.ModelMap["other"])
	}
	if len(saved.ModelFallback) != 1 {
		t.Fatalf("降级链条目丢了：%+v", saved.ModelFallback)
	}
	r := saved.ModelFallback[0]
	if r.From != "stepfun2/step-5" || r.To[0] != "stepfun2/glm-4" || r.To[1] != "workbuddy/glm-5.2" {
		t.Errorf("降级链未（正确）联动：%+v", r)
	}
	if saved.ReasoningEfforts["stepfun2/step-5"] != "high" {
		t.Errorf("思考强度映射未按新键改写：%v", saved.ReasoningEfforts)
	}
	if _, bad := saved.ReasoningEfforts["stepfun/step-5"]; bad {
		t.Errorf("旧键未删除，会与新键并存打架：%v", saved.ReasoningEfforts)
	}
	if saved.ReasoningEfforts["workbuddy/x"] != "low" || saved.ReasoningEfforts["*"] != "medium" {
		t.Errorf("无关档位被牵连：%v", saved.ReasoningEfforts)
	}

	// ③ 旧 state 文件留档
	if _, err := os.Stat(oldState); !os.IsNotExist(err) {
		t.Errorf("旧 state 文件应已改名，实际仍在：%v", err)
	}
	if _, err := os.Stat(oldState + ".old"); err != nil {
		t.Errorf("旧 state 文件未留 .old 档：%v", err)
	}
	// ④ syncer 必须被推（运行期渠道表与降级链内存副本都要跟上）
	if pushed == 0 || fbPushed == 0 {
		t.Errorf("改名后未推送运行期同步：channels=%d fallback=%d", pushed, fbPushed)
	}
	// ⑤ 费率页/状态页的回显按新名仍然拿得到（渠道表已换）
	if got := a.resolveCustomKey("STEPFUN2"); got != "sk-real-secret-1234" {
		t.Errorf("新名取真值失败：%q", got)
	}
	if got := a.resolveCustomKey("stepfun"); got != "" {
		t.Errorf("旧名不该还能取到值（会掩盖改名没生效的 bug）：%q", got)
	}
}

// TestCustomChannelRenameInvalidDropped 非法改名声明必须被丢弃而不是把配置搅坏：
// 旧名不存在（可能是面板残留的陈旧 pendingRenames）、新名已存在、新旧同名，都不参与级联。
func TestCustomChannelRenameInvalidDropped(t *testing.T) {
	tmpDir := appTempDir(t)
	cfg := config.Default()
	cfg.StateFile = tmpDir + "/state.json"
	cfgPath := tmpDir + "/config.json"
	cfg.CustomChannels = []custom.ChannelConfig{
		{Name: "aa", BaseURL: "https://a.com/v1", APIKey: "sk-aa-1234567890"},
		{Name: "bb", BaseURL: "https://b.com/v1", APIKey: "sk-bb-1234567890"},
	}
	cfg.Compat.ModelMap = map[string]string{"m": "aa/m"}
	_ = config.Save(cfg, cfgPath)
	a, err := New(Options{ConfigPath: cfgPath, Config: cfg, Runtimes: map[provider.Kind]*Runtime{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()

	// 三条非法声明：旧名不存在 / 新名撞已有渠道 / 新旧相同；提交的是 aa 的脱敏回显（名字没改）
	shown := a.GetState().CustomChannels
	maskAA := ""
	maskBB := ""
	for _, c := range shown {
		if c.Name == "aa" {
			maskAA = c.APIKey
		}
		if c.Name == "bb" {
			maskBB = c.APIKey
		}
	}
	err = a.SetCustomChannels(
		[]custom.ChannelConfig{
			{Name: "aa", BaseURL: "https://a.com/v1", APIKey: maskAA},
			{Name: "bb", BaseURL: "https://b.com/v1", APIKey: maskBB},
		},
		customRename{New: "cc", Old: "nonexistent"},
		customRename{New: "bb", Old: "aa"}, // bb 已存在 ⇒ 丢弃（否则 bb 的 key 会被 aa 的顶掉）
		customRename{New: "aa", Old: "aa"},
	)
	if err != nil {
		t.Fatalf("非法改名应被静默丢弃而不是报错: %v", err)
	}
	saved, _ := config.Load(cfgPath)
	if saved.CustomChannels[0].APIKey != "sk-aa-1234567890" || saved.CustomChannels[1].APIKey != "sk-bb-1234567890" {
		t.Errorf("两个渠道的真实密钥被搅了：%+v", saved.CustomChannels)
	}
	if saved.Compat.ModelMap["m"] != "aa/m" {
		t.Errorf("无效改名却改写了引用表：%q", saved.Compat.ModelMap["m"])
	}
}
