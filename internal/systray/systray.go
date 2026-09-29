// Package systray 系统托盘封装：固定菜单（打开主界面 / 查看日志 / 退出）。
// 基于 energye/systray（跨平台 Windows/macOS/Linux），单文件实现，无平台差异代码。
//
// 设计约定（见项目 AGENTS.md R1-R3）：
//   - 菜单固定，不做动态内容，不用定时/事件刷新
//   - 单击/双击托盘 = 打开主界面
package systray

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/energye/systray"
)

// 图标回收信号：doneCh 在托盘消息循环里执行完 Shell_NotifyIcon(NIM_DELETE) 后关闭。
var (
	trayStarted atomic.Bool
	doneOnce    sync.Once
	doneCh      = make(chan struct{})
)

// Actions 托盘动作回调（由 daemon 注入）。
type Actions struct {
	// OpenUI 打开主界面（系统浏览器）。
	OpenUI func()
	// OpenLog 打开日志文件。
	OpenLog func()
	// Quit 退出程序。
	Quit func()
}

// menuIcon 生成 16x16 菜单项图标：白边纯色方块，包成单条目 ICO。
//
// 为什么必须包成 ICO：Windows 侧 MenuItem.SetIcon 走 LoadImage(IMAGE_ICON,
// LR_LOADFROMFILE)，只认 .ico/.bmp 容器，直接喂裸 PNG 一定失败（日志
// "unable to load icon from temp file"），菜单项图标就永远不显示。条目内用 PNG
// 压缩（Vista+ 支持），与 build/trayicon.ico 同一种形式（见 cmd/genicon）。
func menuIcon(r, g, b uint8) []byte {
	const size = 16
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if y == 0 || y == size-1 || x == 0 || x == size-1 {
				img.Set(x, y, color.RGBA{255, 255, 255, 255}) // 白色边框
				continue
			}
			img.Set(x, y, color.RGBA{r, g, b, 255})
		}
	}
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		log.Printf("systray error: 生成菜单图标失败: %s", err)
		return nil
	}
	data := pngBuf.Bytes()

	var buf bytes.Buffer
	// ICONDIR：reserved / type=1(icon) / count=1
	binary.Write(&buf, binary.LittleEndian, uint16(0))
	binary.Write(&buf, binary.LittleEndian, uint16(1))
	binary.Write(&buf, binary.LittleEndian, uint16(1))
	// ICONDIRENTRY（16 字节）：宽 / 高 / 调色板数 / reserved / planes / bitcount / 长度 / 偏移
	buf.WriteByte(size)                                        // 宽（256 才记 0，16 直接写尺寸）
	buf.WriteByte(size)                                        // 高
	buf.WriteByte(0)                                           // 调色板数（真彩为 0）
	buf.WriteByte(0)                                           // reserved
	binary.Write(&buf, binary.LittleEndian, uint16(1))         // planes
	binary.Write(&buf, binary.LittleEndian, uint16(32))        // bitcount
	binary.Write(&buf, binary.LittleEndian, uint32(len(data))) // 数据长度
	binary.Write(&buf, binary.LittleEndian, uint32(6+16))      // 数据偏移
	buf.Write(data)
	return buf.Bytes()
}

// Run 启动托盘（阻塞，直到 Quit 或托盘消息循环结束）。icon 为 ico/png 字节。
func Run(icon []byte, tooltip string, act Actions) {
	if act.OpenUI == nil {
		act.OpenUI = func() {}
	}
	if act.OpenLog == nil {
		act.OpenLog = func() {}
	}
	if act.Quit == nil {
		act.Quit = func() {}
	}

	// 各菜单项图标（16x16 纯色小方块）
	iconOpen := menuIcon(37, 99, 235)  // 蓝色 - 打开
	iconLog := menuIcon(140, 145, 159) // 灰色 - 日志
	iconQuit := menuIcon(220, 38, 38)  // 红色 - 退出

	trayStarted.Store(true)
	systray.Run(func() {
		systray.SetIcon(icon)
		systray.SetTooltip(tooltip)

		mOpen := systray.AddMenuItem("打开主界面", "在系统浏览器中打开管理界面")
		mOpen.SetIcon(iconOpen)

		mLog := systray.AddMenuItem("查看日志", "用系统默认编辑器打开日志文件")
		mLog.SetIcon(iconLog)

		systray.AddSeparator()

		mQuit := systray.AddMenuItem("退出", "退出程序")
		mQuit.SetIcon(iconQuit)

		// 单击 / 双击 = 打开主界面（与旧版行为一致）
		systray.SetOnClick(func(systray.IMenu) { go act.OpenUI() })
		systray.SetOnDClick(func(systray.IMenu) { go act.OpenUI() })

		// 菜单回调：全部 goroutine 化（托盘消息循环线程只做投递，绝不阻塞）
		mOpen.Click(func() { go act.OpenUI() })
		mLog.Click(func() { go act.OpenLog() })
		mQuit.Click(func() { go act.Quit() })
	}, func() {
		log.Printf("托盘已退出")
		doneOnce.Do(func() { close(doneCh) })
	})
	// 消息循环因其它原因结束时同样视为已回收（Quit 不必再等）。
	doneOnce.Do(func() { close(doneCh) })
}

// Quit 摘除托盘图标并等待通知区域回收完成，返回是否确认回收。
//
// 退出进程前必须走这里。托盘图标由 Shell_NotifyIcon(NIM_DELETE) 摘除，而该调用
// 发生在托盘消息循环里；直接 os.Exit 会连同消息循环一起跳过，Windows 任务栏会
// 残留「幽灵图标」，直到鼠标划过该区域才被系统清掉。
//
// 未启动托盘（--no-tray 或初始化失败）时立即返回 true。
func Quit(timeout time.Duration) bool {
	if !trayStarted.Load() {
		return true
	}
	systray.Quit()
	return waitTrayDone(doneCh, timeout)
}

// waitTrayDone 等图标回收信号（done 关闭），超时返回 false。
//
// 拆成独立函数是为了让单测能传局部 channel —— 否则测「已回收」这条路径就得关闭
// 包级 doneCh，用例之间会产生顺序依赖。
func waitTrayDone(done <-chan struct{}, timeout time.Duration) bool {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		log.Printf("托盘图标回收超时（%v），继续退出", timeout)
		return false
	}
}
