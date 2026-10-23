package systray

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// TestMenuIconICOStructure 菜单项图标必须是 ICO 容器，且条目里的 PNG 必须真能解码。
//
// Windows 的 LoadImage(IMAGE_ICON) 不认裸 PNG，所以图标要包成 ICO；而历史上手写的
// PNG 编码器产出的字节根本解不开（png.Decode 报 invalid checksum），于是菜单项
// 图标一直不显示（每次启动 3 条 "unable to load icon from temp file"）。
func TestMenuIconICOStructure(t *testing.T) {
	ico := menuIconICO(iconOpenUI())
	if len(ico) < 22 {
		t.Fatalf("ICO 太短：%d 字节", len(ico))
	}
	if got := binary.LittleEndian.Uint16(ico[0:2]); got != 0 {
		t.Errorf("ICONDIR.reserved 应为 0，得到 %d", got)
	}
	if got := binary.LittleEndian.Uint16(ico[2:4]); got != 1 {
		t.Errorf("ICONDIR.type 应为 1(icon)，得到 %d", got)
	}
	if got := binary.LittleEndian.Uint16(ico[4:6]); got != 1 {
		t.Errorf("ICONDIR.count 应为 1，得到 %d", got)
	}
	const size = 16
	if ico[6] != size || ico[7] != size {
		t.Errorf("条目尺寸应为 %dx%d，得到 %dx%d", size, size, ico[6], ico[7])
	}
	dsize := binary.LittleEndian.Uint32(ico[14:18])
	off := binary.LittleEndian.Uint32(ico[18:22])
	if int(off)+int(dsize) != len(ico) {
		t.Fatalf("数据区不吻合：off=%d size=%d 总长=%d", off, dsize, len(ico))
	}
	if !bytes.Equal(ico[off:off+8], []byte{137, 80, 78, 71, 13, 10, 26, 10}) {
		t.Errorf("条目数据应为 PNG（PNG 签名），实际 % x", ico[off:off+8])
	}
	// 关键断言：条目里的 PNG 必须真能解码，且尺寸正确、含不透明像素（不是全透明空图）。
	img, err := png.Decode(bytes.NewReader(ico[off : off+dsize]))
	if err != nil {
		t.Fatalf("条目数据不是合法 PNG：%v", err)
	}
	if b := img.Bounds(); b.Dx() != size || b.Dy() != size {
		t.Fatalf("解出的尺寸应为 %dx%d，得到 %dx%d", size, size, b.Dx(), b.Dy())
	}
	opaque := 0
	for j := 0; j < size; j++ {
		for i := 0; i < size; i++ {
			if _, _, _, a := img.At(i, j).RGBA(); a == 0xffff {
				opaque++
			}
		}
	}
	if opaque == 0 {
		t.Fatal("条目 PNG 全透明，图形丢失")
	}
}

func rgba(t *testing.T, img *image.RGBA, x, y int) color.RGBA {
	t.Helper()
	return color.RGBAModel.Convert(img.At(x, y)).(color.RGBA)
}

// 图形语义锁死：以下断言锁的是「哪个位置是什么颜色 / 透明」，
// 防止重构时无感改动图形（历史上色块→像素画的重构就动过整个绘制函数）。

// TestIconOpenUIPixels 「打开主界面」= 蓝色外链（方框 + 右上斜箭头）。
func TestIconOpenUIPixels(t *testing.T) {
	img := iconOpenUI()
	if c := rgba(t, img, 2, 8); c != colOpen {
		t.Errorf("盒左边 (2,8) 应为蓝色，得到 %v", c)
	}
	if c := rgba(t, img, 14, 2); c != colOpen {
		t.Errorf("箭头框右上角 (14,2) 应为蓝色，得到 %v", c)
	}
	if c := rgba(t, img, 13, 3); c != colOpen {
		t.Errorf("箭杆斜线末端 (13,3) 应为蓝色，得到 %v", c)
	}
	if _, _, _, a := img.At(6, 9).RGBA(); a != 0 {
		t.Errorf("盒内空白 (6,9) 应透明，得到 alpha=%d", a)
	}
	if _, _, _, a := img.At(0, 0).RGBA(); a != 0 {
		t.Errorf("背景 (0,0) 应透明（不再是白边色块），得到 alpha=%d", a)
	}
}

// TestIconLogDocPixels 「查看日志」= 灰色文档（白纸 + 折角 + 三条文本线）。
func TestIconLogDocPixels(t *testing.T) {
	img := iconLogDoc()
	if c := rgba(t, img, 6, 2); c != colPaper {
		t.Errorf("纸面 (6,2) 应为白色，得到 %v", c)
	}
	if c := rgba(t, img, 4, 8); c != colDocE {
		t.Errorf("纸左边 (4,8) 应为轮廓灰，得到 %v", c)
	}
	if c := rgba(t, img, 10, 3); c != colDocE {
		t.Errorf("折角斜线 (10,3) 应为轮廓灰，得到 %v", c)
	}
	if c := rgba(t, img, 6, 6); c != colDocL {
		t.Errorf("文本线 (6,6) 应为浅灰，得到 %v", c)
	}
	if _, _, _, a := img.At(0, 0).RGBA(); a != 0 {
		t.Errorf("背景 (0,0) 应透明，得到 alpha=%d", a)
	}
}

// TestIconQuitPowerPixels 「退出」= 红色电源符号（圆弧 + 顶部竖线，顶部有缺口）。
func TestIconQuitPowerPixels(t *testing.T) {
	img := iconQuitPower()
	if c := rgba(t, img, 7, 3); c != colQuit {
		t.Errorf("竖线 (7,3) 应为红色，得到 %v", c)
	}
	if c := rgba(t, img, 7, 15); c != colQuit {
		t.Errorf("底部圆弧 (7,15) 应为红色，得到 %v", c)
	}
	if c := rgba(t, img, 3, 8); c != colQuit {
		t.Errorf("左侧圆弧 (3,8) 应为红色，得到 %v", c)
	}
	if _, _, _, a := img.At(7, 8).RGBA(); a != 0 {
		t.Errorf("圆心 (7,8) 应透明，得到 alpha=%d", a)
	}
	if _, _, _, a := img.At(0, 0).RGBA(); a != 0 {
		t.Errorf("背景 (0,0) 应透明，得到 alpha=%d", a)
	}
}
