package systray

import (
	"bytes"
	"encoding/binary"
	"image/color"
	"image/png"
	"testing"
)

// TestMenuIconIsICO 菜单项图标必须是 ICO 容器，且条目里的 PNG 必须真能解码。
//
// Windows 的 LoadImage(IMAGE_ICON) 不认裸 PNG，所以图标要包成 ICO；而此前手写的
// PNG 编码器产出的字节根本解不开（png.Decode 报 invalid checksum），于是菜单项
// 图标一直不显示（每次启动 3 条 "unable to load icon from temp file"）。
func TestMenuIconIsICO(t *testing.T) {
	const size = 16
	ico := menuIcon(37, 99, 235)
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
	// 关键断言：条目里的 PNG 必须真能解码。旧的手写编码器过不了这一关。
	img, err := png.Decode(bytes.NewReader(ico[off : off+dsize]))
	if err != nil {
		t.Fatalf("条目数据不是合法 PNG：%v", err)
	}
	if b := img.Bounds(); b.Dx() != size || b.Dy() != size {
		t.Fatalf("解出的尺寸应为 %dx%d，得到 %dx%d", size, size, b.Dx(), b.Dy())
	}
	corner := color.RGBAModel.Convert(img.At(0, 0)).(color.RGBA)
	if corner.R != 255 || corner.G != 255 || corner.B != 255 {
		t.Errorf("四边应为白色，左上角得到 %v", corner)
	}
	center := color.RGBAModel.Convert(img.At(8, 8)).(color.RGBA)
	if center.R != 37 || center.G != 99 || center.B != 235 {
		t.Errorf("中心应为传入颜色 37,99,235，得到 %v", center)
	}
}
