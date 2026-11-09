package ledger

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestUsageEntryCreditRoundTrip UsageEntry.Credit 必须能落盘并被聚合读回（R130）：
// comate 无余额端点，逐请求 usage.credits 是它唯一可见的消耗口径。
func TestUsageEntryCreditRoundTrip(t *testing.T) {
	dir := t.TempDir()
	l, err := New(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	now := time.Now()
	l.AppendUsage(UsageEntry{Ts: now.Unix(), Ch: "comate", UID: "cm", Model: "glm-5.3-flash", PT: 100, CT: 20, Credit: 0.01})
	l.AppendUsage(UsageEntry{Ts: now.Unix(), Ch: "comate", UID: "cm", Model: "glm-5.3-flash", PT: 200, CT: 40, Credit: 0.02})
	l.Close()

	l2, err := New(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer l2.Close()
	q := l2.Query(1, nil)
	if q == nil {
		t.Fatal("Query 返回 nil")
	}
	var got float64
	for _, m := range q.Token.ByModel {
		if m.Channel == "comate" {
			got = m.Credit
		}
	}
	if got != 0.03 {
		t.Fatalf("ByModel credit = %v, want 0.03（0.01+0.02）", got)
	}
}

// TestUsageEntryCreditOmitEmpty 无积分渠道的行里不得出现 credit 字段：
// 否则「不知道」会被写成「花了 0」，且旧流水口径被污染（不变量 5）。
func TestUsageEntryCreditOmitEmpty(t *testing.T) {
	b, err := json.Marshal(UsageEntry{Ts: 1, Ch: "workbuddy", Model: "m", PT: 1, CT: 1})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "credit") {
		t.Fatalf("无积分行不应带 credit 字段: %s", b)
	}
}
