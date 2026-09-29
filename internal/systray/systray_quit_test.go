package systray

import (
	"testing"
	"time"
)

// 未启动托盘（--no-tray / 初始化失败）时 Quit 立即返回，不拖慢进程退出。
func TestQuitWithoutTray(t *testing.T) {
	start := time.Now()
	if !Quit(5 * time.Second) {
		t.Fatal("未启动托盘时 Quit 应返回 true")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("未启动托盘时不应等待，实际耗时 %v", d)
	}
}

// 消息循环未响应（图标回收未确认）时按超时返回 false，不阻塞退出。
//
// 这条走完整的 Quit 路径（trayStarted=true），顺带覆盖 Quit → waitTrayDone 的接线；
// 只改 trayStarted 且 cleanup 复位，故与用例顺序无关。
func TestQuitTimesOutWithoutLoop(t *testing.T) {
	trayStarted.Store(true)
	t.Cleanup(func() { trayStarted.Store(false) })

	start := time.Now()
	if Quit(150 * time.Millisecond) {
		t.Fatal("消息循环未响应时应返回 false")
	}
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Fatalf("应等待到超时，实际 %v", d)
	}
}

// 图标回收完成后不再等待（done 关闭 = 消息循环执行完 NIM_DELETE）。
//
// 用局部 channel，而不是关闭包级 doneCh —— 后者会让本用例与执行顺序绑定
// （-shuffle 下必红）。
func TestWaitTrayDoneReturnsOnClose(t *testing.T) {
	closed := make(chan struct{})
	close(closed)

	start := time.Now()
	if !waitTrayDone(closed, 5*time.Second) {
		t.Fatal("回收完成后应返回 true")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("回收完成后不应等待，实际耗时 %v", d)
	}
}
