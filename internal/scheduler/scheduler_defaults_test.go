package scheduler

import (
	"context"
	"testing"
	"time"
)

// 零值（nil）表示「未配置」，必须落默认 —— 既有渠道依赖这个行为。
func TestNewZeroValueFallsBackToDefaults(t *testing.T) {
	s := New(Config{})
	ch := s.CheckinMinutes()
	if len(ch) != 2 || ch[0] != 9*60 || ch[1] != 21*60 {
		t.Fatalf("零值 CheckinMinutes = %v，want [540 1260]", ch)
	}
	kh := s.KeepaliveHours()
	if len(kh) != 1 || kh[0] != 22 {
		t.Fatalf("零值 KeepaliveHours = %v，want [22]", kh)
	}
}

// 显式空切片表示「本渠道没有这类定时任务」，不得被补成默认值 ——
// 否则无 refresh 端点 / 无签到活动的渠道会每天空跑并记录失败。
func TestNewExplicitEmptyDisablesTasks(t *testing.T) {
	s := New(Config{CheckinMinutes: []int{}, KeepaliveHours: []int{}})
	if got := s.CheckinMinutes(); len(got) != 0 {
		t.Fatalf("显式空 CheckinMinutes = %v，want 空", got)
	}
	if got := s.KeepaliveHours(); len(got) != 0 {
		t.Fatalf("显式空 KeepaliveHours = %v，want 空", got)
	}

	// 混合场景：签到保留、保活关闭（WorkBuddy 国际版的形态）。
	s2 := New(Config{CheckinMinutes: []int{9 * 60}, KeepaliveHours: []int{}})
	if got := s2.CheckinMinutes(); len(got) != 1 || got[0] != 9*60 {
		t.Fatalf("混合 CheckinMinutes = %v，want [540]", got)
	}
	if got := s2.KeepaliveHours(); len(got) != 0 {
		t.Fatalf("混合 KeepaliveHours = %v，want 空", got)
	}
}

// CheckinHours 是旧配置兼容字段：CheckinMinutes 为 nil 时由它换算。
func TestNewCheckinHoursCompat(t *testing.T) {
	s := New(Config{CheckinHours: []int{7, 19}})
	got := s.CheckinMinutes()
	if len(got) != 2 || got[0] != 7*60 || got[1] != 19*60 {
		t.Fatalf("CheckinHours 兼容换算 = %v，want [420 1140]", got)
	}
}

// 无任何定时任务时 Run 必须阻塞等待，且能响应 ctx 取消。
// 空列表会让 nextFireMinutes 返回零值、timer 立即触发 → 空转烧 CPU，故需专门保护。
func TestRunWithoutTasksBlocksUntilCancel(t *testing.T) {
	s := New(Config{CheckinMinutes: []int{}, KeepaliveHours: []int{}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("无定时任务时 Run 不应自行返回")
	case <-time.After(120 * time.Millisecond):
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ctx 取消后 Run 未退出")
	}
}
