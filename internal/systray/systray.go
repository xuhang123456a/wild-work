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
	"math"
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

// ---------- 菜单项图标：16x16 像素画 ----------
//
// 为什么手绘像素画而不是 emoji / 真实图标：
//  - Win32 菜单文本走 GDI 渲染，不支持 COLR 彩色字体，文字里嵌 emoji 只能画成
//    黑色单色轮廓，且字体回退可能出「豆腐块」；
//  - 彩色 emoji 需要 DirectWrite 渲染 COLR 字形（数百行 Windows 专属代码）或嵌入
//    第三方 PNG 资产，都违背 R6「纯 Go 生成、无外部图标文件」；
//  - 像素画零依赖、跨平台一致，测试可断言具体像素锁死图形语义。
//
// 16x16 条目在 Windows 侧经 LoadImage(LR_DEFAULTSIZE) 16→32 放大、DrawIconEx
// 32→16 缩小，整数倍缩放为最近邻映射，像素画无损复原。透明背景经 32bpp DIB
// （MIIM_BITMAP）保留 alpha，菜单上不会出现黑底。

var (
	colOpen  = color.RGBA{37, 99, 235, 255}   // 蓝 - 打开主界面
	colQuit  = color.RGBA{220, 38, 38, 255}   // 红 - 退出
	colPaper = color.RGBA{255, 255, 255, 255} // 文档纸面
	colDocE  = color.RGBA{110, 115, 125, 255} // 文档轮廓（灰）
	colDocL  = color.RGBA{130, 135, 145, 255} // 文档文本线（浅灰）
)

// iconOpenUI 「打开主界面」：外链图形（方框 + 右上斜箭头），蓝色。
func iconOpenUI() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	c := colOpen
	for j := 4; j <= 14; j++ {
		img.Set(2, j, c) // 盒左边
	}
	for i := 2; i <= 12; i++ {
		img.Set(i, 14, c) // 盒底边
	}
	for i := 2; i <= 7; i++ {
		img.Set(i, 4, c) // 盒顶边左段
	}
	for j := 9; j <= 14; j++ {
		img.Set(12, j, c) // 盒右边下段
	}
	for i := 10; i <= 14; i++ {
		img.Set(i, 2, c) // 箭头框上边
	}
	for j := 2; j <= 6; j++ {
		img.Set(14, j, c) // 箭头框右边
	}
	for k := 0; k <= 6; k++ {
		img.Set(7+k, 9-k, c) // 箭杆斜线 (7,9)→(13,3)
	}
	return img
}

// iconLogDoc 「查看日志」：文档图形（白纸 + 折角 + 三条文本线），灰色调。
func iconLogDoc() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for j := 1; j <= 14; j++ {
		for i := 4; i <= 11; i++ {
			img.Set(i, j, colPaper) // 纸面
		}
	}
	for i := 4; i <= 11; i++ {
		img.Set(i, 14, colDocE) // 底边
	}
	for j := 1; j <= 14; j++ {
		img.Set(4, j, colDocE)  // 左边
		img.Set(11, j, colDocE) // 右边
	}
	for i := 4; i <= 8; i++ {
		img.Set(i, 1, colDocE) // 顶边（到折角）
	}
	for k := 0; k <= 3; k++ {
		img.Set(8+k, 1+k, colDocE) // 折角斜线 (8,1)→(11,4)
	}
	for i := 6; i <= 9; i++ {
		img.Set(i, 6, colDocL)  // 文本线 1
		img.Set(i, 9, colDocL)  // 文本线 2
		img.Set(i, 12, colDocL) // 文本线 3
	}
	return img
}

// iconQuitPower 「退出」：电源符号（圆弧 + 顶部竖线），红色。
func iconQuitPower() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			dx, dy := float64(x)-7.5, float64(y)-8.5
			if d := math.Sqrt(dx*dx + dy*dy); d >= 4.4 && d <= 6.6 {
				// 圆心 (7.5,8.5)，顶部 -90° 附近留缺口给竖线
				if ang := math.Atan2(dy, dx) * 180 / math.Pi; ang > -125 && ang < -55 {
					continue
				}
				img.Set(x, y, colQuit)
			}
		}
	}
	for j := 1; j <= 7; j++ {
		img.Set(7, j, colQuit) // 竖线左列
		img.Set(8, j, colQuit) // 竖线右列
	}
	return img
}

// menuIconICO 把 16x16 RGBA 图包成单条目 ICO（条目内 PNG 压缩，Vista+ 支持）。
//
// Windows 侧 MenuItem.SetIcon 走 LoadImage(IMAGE_ICON, LR_LOADFROMFILE)，只认
// .ico/.bmp 容器，直接喂裸 PNG 一定失败（日志 "unable to load icon from temp
// file"）。与 build/trayicon.ico 同一种形式（见 cmd/genicon）。
func menuIconICO(img *image.RGBA) []byte {
	const size = 16
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

	// 各菜单项图标（16x16 像素画：蓝外链 / 灰文档 / 红电源）
	iconOpen := menuIconICO(iconOpenUI())
	iconLog := menuIconICO(iconLogDoc())
	iconQuit := menuIconICO(iconQuitPower())

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
