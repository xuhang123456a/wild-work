// 工具轮 content 空值修补（normalizeToolTurnContent）的回归测试。
//
// 背景：上游 qoder/deepseek-flash 要求带 tool_calls 的 assistant 消息必须携带
// 字符串 content，null / 缺失会导致整请求被拒，并返回一个误导性错误：
//
//	Messages with role 'tool' must be a response to a preceding message with 'tool_calls'
//
// 实测（2026-09-24）：content="" 正常出流，content=null 或字段缺失必空流。
package server

import (
	"encoding/json"
	"testing"
)

func TestNormalizeToolTurnContentFixesNullAndMissing(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string // 期望 messages[0].content 的类型："str" / "nil"
	}{
		{
			name: "assistant with tool_calls and null content",
			src:  `{"messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"Bash","arguments":"{}"}}],"content":null}]}`,
			want: "str",
		},
		{
			name: "assistant with tool_calls and missing content",
			src:  `{"messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"Bash","arguments":"{}"}}]}]}`,
			want: "str",
		},
		{
			name: "tool message with null content",
			src:  `{"messages":[{"role":"tool","tool_call_id":"c1","content":null}]}`,
			want: "str",
		},
		{
			// 普通 assistant 消息不动：无证据表明上游对此有要求，不做无谓改写。
			name: "assistant without tool_calls keeps null",
			src:  `{"messages":[{"role":"assistant","content":null}]}`,
			want: "nil",
		},
		{
			name: "user message keeps null",
			src:  `{"messages":[{"role":"user","content":null}]}`,
			want: "nil",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := rewriteModel([]byte(tc.src), "qoder/deepseek-flash")
			if err != nil {
				t.Fatalf("rewriteModel: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			msgs, _ := got["messages"].([]any)
			if len(msgs) != 1 {
				t.Fatalf("messages 数量 = %d, want 1", len(msgs))
			}
			m, _ := msgs[0].(map[string]any)
			c, exists := m["content"]
			switch tc.want {
			case "str":
				s, ok := c.(string)
				if !ok || s != "" {
					t.Fatalf("content = %#v (exists=%v), want 空字符串", c, exists)
				}
			case "nil":
				if exists && c != nil {
					t.Fatalf("content = %#v, want 保持 null/缺失", c)
				}
			}
		})
	}
}

// 真实故障请求的最小化形态：修补后必须让 assistant 的 content 变成字符串，
// 其余字段（tool_calls 的 id、tool_call_id 配对）原样保留。
func TestNormalizeToolTurnContentKeepsToolCallPairing(t *testing.T) {
	src := `{"model":"qoder/deepseek-flash","messages":[
		{"role":"user","content":"list files"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"a.txt"}
	]}`
	raw, err := rewriteModel([]byte(src), "qoder/deepseek-flash")
	if err != nil {
		t.Fatalf("rewriteModel: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs, _ := got["messages"].([]any)
	asst, _ := msgs[1].(map[string]any)
	c, isStr := asst["content"].(string)
	if !isStr || c != "" {
		t.Fatalf("assistant.content = %#v, want \"\"", asst["content"])
	}
	tcs, _ := asst["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls 丢失：%#v", asst["tool_calls"])
	}
	tc, _ := tcs[0].(map[string]any)
	if tc["id"] != "call_1" {
		t.Fatalf("tool_call id 变化：%#v", tc["id"])
	}
	tool, _ := msgs[2].(map[string]any)
	if tool["tool_call_id"] != "call_1" {
		t.Fatalf("tool_call_id 变化：%#v", tool["tool_call_id"])
	}
}
