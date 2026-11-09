// 降级链别名（R129）在 Chat 接口的端到端行为锁定：别名是主力模型的另一个名字，
// 客户端请求别名 == 请求主力模型（粘性路由 / 容量判定 / 降级链 / 冷却全部照旧）。
//
// 只断言外部可观察行为：HTTP 请求进去 → 状态码 / 响应头 / 响应体出来；落盘流水里是什么。
// 不断言内部函数名与 map 被赋了几次值。
package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"wild-work/internal/config"
	"wild-work/internal/ledger"
	"wild-work/internal/oczen"
	"wild-work/internal/stats"
)

// newAliasHandler 装配两个渠道（oczen=主、workbuddy=备）+ 一条带别名的降级规则。
func newAliasHandler(t *testing.T, upA, upB *effUpstream, name string, efforts map[string]string) *Handler {
	t.Helper()
	rule := config.ModelFallbackRule{From: "oczen/big-pickle", To: []string{"workbuddy/glm-free"}, Name: name}
	h, _, _ := newFbHandler(t, upA, upB, []config.ModelFallbackRule{rule})
	h.SetReasoningEfforts(efforts)
	return h
}

// TestAliasResolvesToPrimary 别名命中：上游收到的是主力模型名，响应体 model 回填别名
// （客户端视角的请求名），且不产生降级标注。
func TestAliasResolvesToPrimary(t *testing.T) {
	upA, upB := &effUpstream{}, &effUpstream{}
	h := newAliasHandler(t, upA, upB, "fast", nil)

	rec := callModel(h, "fast")
	if rec.Code != http.StatusOK {
		t.Fatalf("别名请求应 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if upA.calls != 1 || upB.calls != 0 {
		t.Fatalf("别名应路由到主力渠道：upA=%d upB=%d，want 1/0", upA.calls, upB.calls)
	}
	var sent map[string]any
	if err := json.Unmarshal(upA.lastBody(), &sent); err != nil {
		t.Fatalf("上游请求体不是合法 JSON: %v", err)
	}
	if sent["model"] != "big-pickle" {
		t.Errorf("上游收到的 model = %v，want 主力的上游名 big-pickle", sent["model"])
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["model"] != "fast" {
		t.Errorf("响应体 model = %v，want 客户端请求名 fast", resp["model"])
	}
	if got := rec.Header().Get("X-Model-Fallback"); got != "" {
		t.Errorf("无降级时不得带 X-Model-Fallback，实际 %q", got)
	}
}

// TestAliasTriggersFallbackChainWithAliasInHeader 别名请求触发降级链：备胎顶上，
// 响应头与降级记录事件用**客户端请求名（= 别名）**，响应体 model 回填备胎全路径。
func TestAliasTriggersFallbackChainWithAliasInHeader(t *testing.T) {
	st, err := stats.New(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	upA, upB := &effUpstream{}, &effUpstream{}
	h, pa, _ := newFbHandler(t, upA, upB, []config.ModelFallbackRule{
		{From: "oczen/big-pickle", To: []string{"workbuddy/glm-free"}, Name: "fast"},
	})
	h.stats = st
	// 主力对 big-pickle 模型级冷却（账号本身健康）⇒ 入口降级命中
	pa.CooldownModel(oczen.AnonymousUID, "big-pickle", 10*time.Minute, "test")

	rec := callModel(h, "fast")
	if rec.Code != http.StatusOK {
		t.Fatalf("别名请求应降级返回 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Model-Fallback"); got != "fast -> workbuddy/glm-free" {
		t.Errorf("X-Model-Fallback = %q，want 用客户端请求名 fast", got)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["model"] != "workbuddy/glm-free" {
		t.Errorf("降级后响应 model = %v，want 备胎全路径", resp["model"])
	}
	evs := st.Snapshot(time.Now()).Fallbacks
	if len(evs) != 1 || evs[0].From != "fast" || evs[0].To != "workbuddy/glm-free" {
		t.Fatalf("降级记录事件 from 应为别名 fast、to 为备胎，实际 %+v", evs)
	}
	if upB.calls != 1 || upA.calls != 0 {
		t.Errorf("upA=%d upB=%d，want 0/1（降级到备胎）", upA.calls, upB.calls)
	}
}

// TestAliasBookedUnderPrimaryCanonicalID 别名请求的用量流水 / 请求日志 / 模型统计
// 一律记**主力模型的规范对外 ID**，用量榜不出现别名（账本完全透明）。
func TestAliasBookedUnderPrimaryCanonicalID(t *testing.T) {
	st, err := stats.New(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ledgerDir := t.TempDir()
	ld, err := ledger.New(ledgerDir)
	if err != nil {
		t.Fatal(err)
	}
	upA, upB := &effUpstream{}, &effUpstream{}
	h := newAliasHandler(t, upA, upB, "fast", nil)
	h.stats, h.ledger = st, ld

	rec := callModel(h, "fast")
	if rec.Code != http.StatusOK {
		t.Fatalf("别名请求应 200，实际 %d", rec.Code)
	}
	ld.Flush()
	defer ld.Close()

	rows := st.Logs(10)
	if len(rows) != 1 || rows[0].Model != "oczen/big-pickle" {
		t.Fatalf("请求日志 Model 应为主力规范 ID oczen/big-pickle，实际 %+v", rows)
	}
	ues := readUsageToday(t, ledgerDir)
	if len(ues) != 1 || ues[0].Model != "big-pickle" || ues[0].Ch != "oczen" {
		t.Fatalf("流水应为 Model=big-pickle Ch=oczen（不得记别名），实际 %+v", ues)
	}
	for _, m := range st.Snapshot(time.Now()).Models {
		if m.Model == "fast" {
			t.Fatalf("模型统计不得出现别名行，实际 %+v", st.Snapshot(time.Now()).Models)
		}
	}
}

// TestAliasEffortUsesPrimaryConfig 思考强度按**主力模型**查表：为别名单独配的档位不生效，
// 为主力配的档位生效（不为别名单配档位）。
func TestAliasEffortUsesPrimaryConfig(t *testing.T) {
	upA, upB := &effUpstream{}, &effUpstream{}
	h := newAliasHandler(t, upA, upB, "fast", map[string]string{
		"fast":             "low",
		"oczen/big-pickle": "high",
	})
	if rec := callModel(h, "fast"); rec.Code != http.StatusOK {
		t.Fatalf("别名请求应 200，实际 %d", rec.Code)
	}
	val, ok := upstreamEffort(t, upA)
	if !ok || val != "high" {
		t.Errorf("reasoning_effort = %v (exists=%v)，want 按主力模型配置注入 high", val, ok)
	}
}

// TestAliasInvalidNameIsIgnored 手改 config.json 造出的脏别名（含 `/`）在派生时被跳过：
// 不得崩溃、不得误路由（那个名字不会被当成别名解析）。
func TestAliasInvalidNameIsIgnored(t *testing.T) {
	upA, upB := &effUpstream{}, &effUpstream{}
	h, _, _ := newFbHandler(t, upA, upB, []config.ModelFallbackRule{
		{From: "oczen/big-pickle", To: []string{"workbuddy/glm-free"}, Name: "oczen/evil"},
	})
	if got := h.aliasTarget("oczen/evil"); got != "" {
		t.Fatalf("含 / 的脏别名必须被跳过，实际索引到 %q", got)
	}
	// 裸名 `evil` 不得因为脏数据而变成可用别名（无前缀 + 无 default_channel ⇒ 400）
	if rec := callModel(h, "evil"); rec.Code != http.StatusBadRequest {
		t.Fatalf("脏别名不得造出可用的裸名，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if upA.calls != 0 || upB.calls != 0 {
		t.Errorf("脏别名不得触发任何上游请求，实际 upA=%d upB=%d", upA.calls, upB.calls)
	}
}

// TestAliasUnknownModelHintPointsToModels 未命中任何模型/别名时的报错形状与现有未知模型
// 错误同构（400 + invalid_model），且在有别名时提示去 /v1/models 查。
func TestAliasUnknownModelHintPointsToModels(t *testing.T) {
	upA, upB := &effUpstream{}, &effUpstream{}
	h := newAliasHandler(t, upA, upB, "fast", nil)
	rec := callModel(h, "faast")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未知名字应 400，实际 %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "invalid_model") || !strings.Contains(body, "alias") {
		t.Errorf("错误体应含 invalid_model 与别名提示，实际 %s", body)
	}
}
