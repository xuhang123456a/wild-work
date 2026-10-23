package custom

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 上游 /models 各家形状的真实样本单测（R119）。
//
// 为什么全用 parseModelsPayload 而不是起 http server：宽容解析的全部风险都在
// 「字段猜错/漏猜」，纯函数能逐形态定点断言；网络侧只留一个 TestConnection 用例兜底。

func parseT(t *testing.T, body string) []ModelCap {
	t.Helper()
	caps, err := parseModelsPayload([]byte(body))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	return caps
}

func findCap(_ *testing.T, list []ModelCap, id string) *ModelCap {
	for i := range list {
		if list[i].ID == id {
			return &list[i]
		}
	}
	return nil
}

// TestParseOfficialOpenAI 官方形状：只有 id/created/owned_by ⇒ 一项都不落 CapSource，
// 前端据此标「未探测」，绝不拿默认值冒充声明。
func TestParseOfficialOpenAI(t *testing.T) {
	list := parseT(t, `{"object":"list","data":[
	  {"id":"gpt-4o-mini","object":"model","created":1721172342,"owned_by":"openai"}]}`)
	if len(list) != 1 {
		t.Fatalf("模型数 = %d, want 1", len(list))
	}
	c := list[0]
	if c.ID != "gpt-4o-mini" || c.ContextWindow != 0 || c.MaxTokens != 0 {
		t.Fatalf("形状不符：%+v", c)
	}
	if c.CapSource != "" {
		t.Fatalf("无能力字段却标了来源：%q", c.CapSource)
	}
	if c.SupportsTools != nil {
		t.Fatalf("工具位应为未声明，got %v", *c.SupportsTools)
	}
	if c.hasAnyCap() {
		t.Fatalf("hasAnyCap 误判为真：%+v", c)
	}
}

// TestParseNewAPIStyle new-api/one-api 常见私扩：顶层 context_window/max_tokens + 显式能力布尔。
func TestParseNewAPIStyle(t *testing.T) {
	list := parseT(t, `{"data":[
	  {"id":"deepseek-v4.1-flash","context_window":1048576,"max_tokens":65536,
	   "supports_images":true,"supports_reasoning":false,"supports_tools":true,
	   "reasoning_efforts":["Low","HIGH"],"default_reasoning_effort":"HIGH"}]}`)
	c := findCap(t, list, "deepseek-v4.1-flash")
	if c == nil {
		t.Fatalf("没取到条目：%+v", list)
	}
	if c.ContextWindow != 1048576 || c.MaxTokens != 65536 {
		t.Fatalf("容量解析错：%+v", c)
	}
	if !c.SupportsImages {
		t.Fatalf("识图应为真")
	}
	if c.SupportsReasoning {
		t.Fatalf("显式 false 被当成未声明")
	}
	if c.SupportsTools == nil || !*c.SupportsTools {
		t.Fatalf("工具位 = %v, want true", c.SupportsTools)
	}
	// 档位统一小写；默认档位 "high" 在表内才保留
	if strings.Join(c.ReasoningEfforts, ",") != "low,high" || c.DefaultReasoningEffort != "high" {
		t.Fatalf("档位 = %v default=%q", c.ReasoningEfforts, c.DefaultReasoningEffort)
	}
	if c.CapSource != CapSourceAPI {
		t.Fatalf("来源 = %q, want api", c.CapSource)
	}
}

// TestParseOpenRouterStyle OpenRouter 形状：architecture + supported_parameters + top_provider。
func TestParseOpenRouterStyle(t *testing.T) {
	list := parseT(t, `{"data":[{
	  "id":"x/y","name":"X Y","created":1,"description":"d",
	  "architecture":{"input_modalities":["text","image"],"output_modalities":["text"],"tokenizer":"Other"},
	  "top_provider":{"context_length":256000,"max_completion_tokens":64000},
	  "supported_parameters":["tools","temperature","reasoning","include_reasoning"],
	  "supported_reasoning_levels":[{"style":"openai","values":[{"name":"Low"},{"name":"High"}]}]}]}`)
	c := findCap(t, list, "x/y")
	if c == nil {
		t.Fatalf("没取到条目：%+v", list)
	}
	if c.DisplayName != "X Y" {
		t.Fatalf("展示名 = %q", c.DisplayName)
	}
	if c.ContextWindow != 256000 || c.MaxTokens != 64000 {
		t.Fatalf("容量 = %d/%d, want 256000/64000", c.ContextWindow, c.MaxTokens)
	}
	if !c.SupportsImages {
		t.Fatalf("input_modalities 里的 image 应命中识图")
	}
	if !c.SupportsReasoning {
		t.Fatalf("supported_parameters 里的 reasoning 应命中思考")
	}
	if c.SupportsTools == nil || !*c.SupportsTools {
		t.Fatalf("supported_parameters 里的 tools 应命中工具")
	}
	// values 嵌套不在本表覆盖面的对象里，档位取不到是允许的（不得因此判失败），但绝不能编造
	if strings.Join(c.ReasoningEfforts, ",") == "low,high" && c.DefaultReasoningEffort != "" {
		t.Fatalf("默认档位无声明却被填上：%q", c.DefaultReasoningEffort)
	}
}

// TestParseNestedResult 网关把列表再套一层（{"result":{"models":[...]}}）也要能吃。
func TestParseNestedResult(t *testing.T) {
	list := parseT(t, `{"result":{"models":[{"model_id":"a-b","properties":{"context_window":"128k","max_tokens":"8k"},"capabilities":{"image_input":true,"tools":false}}]}}`)
	c := findCap(t, list, "a-b")
	if c == nil {
		t.Fatalf("嵌套形态没取到条目：%+v", list)
	}
	if c.ContextWindow != 128000 || c.MaxTokens != 8000 {
		t.Fatalf("带单位字符串解析错：%+v", c)
	}
	if !c.SupportsImages {
		t.Fatalf("capabilities 对象里 true 的键名应命中")
	}
	if c.SupportsTools == nil || *c.SupportsTools {
		t.Fatalf("capabilities.tools=false 应记为「声明不支持」，got %v", c.SupportsTools)
	}
}

// TestFalseNotMatchedByBlob 关键护栏：capabilities 桶里的布尔 false 不得进子串匹配文本，
// 否则 "supports_images": false 会因为键名含 image 被误判成「支持识图」。
func TestFalseNotMatchedByBlob(t *testing.T) {
	m := map[string]any{"capabilities": map[string]any{"image_input": false, "tools": true}}
	blob := capabilityBlob(m, listKeys)
	if strings.Contains(blob, "image_input") {
		t.Fatalf("false 的键名漏进 blob：%q", blob)
	}
	if !strings.Contains(blob, "tools") {
		t.Fatalf("true 的键名应进 blob：%q", blob)
	}
	list := parseT(t, `{"data":[{"id":"m","capabilities":{"vision":false,"reasoning":false}}]}`)
	if list[0].SupportsImages || list[0].SupportsReasoning {
		t.Fatalf("显式 false 被当成支持：%+v", list[0])
	}
}

// TestNoIdEntrySkipped 没有 id 的条目跳过；全是无 id 则整次探测报错（不得返回空列表假装成功）。
func TestNoIdEntrySkipped(t *testing.T) {
	list := parseT(t, `{"data":[{"id":"ok"},{"name":"无名"},{"oops":1}]}`)
	if len(list) != 1 || list[0].ID != "ok" {
		t.Fatalf("应只保留有 id 的条目：%+v", list)
	}
	if _, err := parseModelsPayload([]byte(`{"data":[{"foo":1}]}`)); err == nil {
		t.Fatalf("无可用条目必须报错")
	}
	if _, err := parseModelsPayload([]byte(`not json`)); err == nil {
		t.Fatalf("非法 JSON 必须报错")
	}
}

// TestDefaultEffortContradiction 上游自相矛盾（默认档位不在档位表里）⇒ 丢弃默认档位，
// 宁可不写也不能拿它去覆盖一个有效档位（出站会按 default 注入强度）。
func TestDefaultEffortContradiction(t *testing.T) {
	c := parseT(t, `{"data":[{"id":"m","reasoning_efforts":["low","high"],"default_reasoning_effort":"ultra"}]}`)[0]
	if c.DefaultReasoningEffort != "" {
		t.Fatalf("矛盾默认档位未丢弃：%q", c.DefaultReasoningEffort)
	}
	if strings.Join(c.ReasoningEfforts, ",") != "low,high" {
		t.Fatalf("有效档位表被误删：%v", c.ReasoningEfforts)
	}
}

// TestThinkingOnlyMarker 只有 "支持思考" 的标记、没有档位表 ⇒ 支持位为真但档位保持未声明。
func TestThinkingOnlyMarker(t *testing.T) {
	c := parseT(t, `{"data":[{"id":"m","features":["thinking"],"context_length":"1M"}]}`)[0]
	if !c.SupportsReasoning {
		t.Fatalf("thinking 标记应命中思考")
	}
	if len(c.ReasoningEfforts) != 0 {
		t.Fatalf("无档位声明却编造出 %v", c.ReasoningEfforts)
	}
	if c.ContextWindow != 1000000 {
		t.Fatalf("1M 应解析成 1000000，got %d", c.ContextWindow)
	}
	if c.CapSource != CapSourceAPI {
		t.Fatalf("来源 = %q", c.CapSource)
	}
}

// TestToModelConfigKeepsUnknown 搬运不得补默认值；未声明的工具位保持 nil（下游按支持处理）。
func TestToModelConfigKeepsUnknown(t *testing.T) {
	c := parseT(t, `{"data":[{"id":"m","context_window":"64k"}]}`)[0]
	mc := c.ToModelConfig()
	if mc.ContextWindow != 64000 || mc.MaxTokens != 0 || mc.SupportsImages || mc.SupportsReasoning {
		t.Fatalf("补了默认值：%+v", mc)
	}
	if mc.SupportsTools != nil {
		t.Fatalf("工具位应为 nil")
	}
	if mc.CapSource != CapSourceAPI {
		t.Fatalf("来源丢失：%+v", mc)
	}
	empty := ModelCap{ID: "x"}.ToModelConfig()
	if empty.CapSource != "" || empty.HasAnyCap() {
		t.Fatalf("纯 ID 条目应落「未探测」：%+v", empty)
	}
}

// TestProbeJSONFieldNames 前端直接吃 caps 数组，字段名必须与 ModelConfig 逐项一致。
func TestProbeJSONFieldNames(t *testing.T) {
	b, err := json.Marshal(ModelCap{ID: "m", ContextWindow: 1, MaxTokens: 2, SupportsImages: true,
		SupportsReasoning: true, ReasoningEfforts: []string{"low"}, CapSource: CapSourceAPI})
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, k := range []string{`"id"`, `"context_window"`, `"max_tokens"`, `"supports_images"`,
		`"supports_reasoning"`, `"reasoning_efforts"`, `"cap_source":"api"`} {
		if !strings.Contains(got, k) {
			t.Fatalf("caps JSON 缺 %s：%s", k, got)
		}
	}
	var back ModelConfig
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.ID != "m" || back.ContextWindow != 1 || back.MaxTokens != 2 || !back.CapFromAPI() {
		t.Fatalf("同一套 json 名没能直接解进 ModelConfig：%+v", back)
	}
}

// TestProbeHTTP 网络侧兜底：Authorization 要带上、非 200 要报错、路径拼接受尾部斜杠。
func TestProbeHTTP(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"m1","context_window":2048}]}`))
	}))
	defer srv.Close()

	c := New(ChannelConfig{Name: "t", BaseURL: srv.URL + "/", APIKey: "sk-test",
		Models: []ModelConfig{{ID: "m1"}}})
	caps, err := c.TestConnection()
	if err != nil {
		t.Fatalf("探测失败: %v", err)
	}
	if len(caps) != 1 || caps[0].ContextWindow != 2048 {
		t.Fatalf("结果不符：%+v", caps)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotPath != "/models" {
		t.Fatalf("路径 = %q（尾部斜杠应被吃掉）", gotPath)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid key"}`))
	}))
	defer bad.Close()
	bc := New(ChannelConfig{Name: "b", BaseURL: bad.URL, Models: []ModelConfig{{ID: "m"}}})
	if _, err := bc.TestConnection(); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("非 200 应带状态码报错，got %v", err)
	}
}

// TestStaticModelsCapSource 修复「自定义渠道上下文列恒不显示」：
// 能力来源必须映射到 ModelInfo 的三个位上，且只有真声明过才置位。
func TestStaticModelsCapSource(t *testing.T) {
	c := New(ChannelConfig{Name: "t", BaseURL: "https://x.example/v1", Models: []ModelConfig{
		{ID: "from-api", ContextWindow: 1000000, CapSource: CapSourceAPI},
		{ID: "from-user", ContextWindow: 64000, MaxTokens: 4096, CapSource: CapSourceUser},
		{ID: "unknown", ContextWindow: 1000000}, // 老配置里的编造值：不得再冒充上游声明
	}})
	byID := map[string]bool{}
	userID := map[string]bool{}
	for _, mi := range c.StaticModels() {
		byID[mi.ID] = mi.ContextFromAPI
		userID[mi.ID] = mi.ContextFromUser
	}
	if !byID["from-api"] || userID["from-api"] {
		t.Fatalf("api 来源映射错：%+v", byID)
	}
	if byID["from-user"] || !userID["from-user"] {
		t.Fatalf("user 来源映射错：%+v", userID)
	}
	if byID["unknown"] || userID["unknown"] {
		t.Fatalf("未探测的来源被当成声明：%+v %+v", byID, userID)
	}
}
