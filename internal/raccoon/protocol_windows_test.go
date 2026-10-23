//go:build windows

package raccoon

import (
	"strings"
	"testing"

	"golang.org/x/sys/windows/registry"
)

// 本文件只操作 HKCU\Software\Classes\wildwork-proto-test-* 这类临时键，
// 与真实的 office-raccoon 协议键完全隔离；每个用例结束时整棵删除。

func testKeyPath(t *testing.T) string {
	t.Helper()
	return `Software\Classes\wildwork-proto-test-` + strings.ReplaceAll(t.Name(), "/", "_")
}

func cleanupKey(t *testing.T, keyPath string) {
	t.Helper()
	t.Cleanup(func() { _ = deleteTree(keyPath, "") })
}

// seedOfficialLike 造一个与官方客户端写入形态一致的键树
// （根键含 (Default) + URL Protocol，另有 shell\open\command）。
func seedOfficialLike(t *testing.T, keyPath, clientExe string) {
	t.Helper()
	root, _, err := registry.CreateKey(registry.CURRENT_USER, keyPath, registry.ALL_ACCESS)
	if err != nil {
		t.Fatalf("建测试键：%v", err)
	}
	if err := root.SetStringValue("", "URL:"+ProtocolScheme); err != nil {
		t.Fatalf("写根键默认值：%v", err)
	}
	if err := root.SetStringValue("URL Protocol", ""); err != nil {
		t.Fatalf("写 URL Protocol：%v", err)
	}
	_ = root.Close()

	ck, _, err := registry.CreateKey(registry.CURRENT_USER,
		keyPath+`\shell\open\command`, registry.ALL_ACCESS)
	if err != nil {
		t.Fatalf("建 command 键：%v", err)
	}
	if err := ck.SetStringValue("", "\""+clientExe+"\" \"%1\""); err != nil {
		t.Fatalf("写 command：%v", err)
	}
	_ = ck.Close()
}

// TestProtocolHijackRestoreRoundTrip 核心用例：备份 → 改写 → 恢复必须逐值还原，
// 包括键树结构（shell / open / command 三个中间键都是 CreateKey 自动建的）。
func TestProtocolHijackRestoreRoundTrip(t *testing.T) {
	keyPath := testKeyPath(t)
	cleanupKey(t, keyPath)
	const clientExe = `C:\Program Files\raccoon-ai\商汤小浣熊.exe`
	seedOfficialLike(t, keyPath, clientExe)

	const ourExe = `D:\tools\wild-work.exe`
	b, err := captureBackup(keyPath, ourExe)
	if err != nil {
		t.Fatalf("captureBackup: %v", err)
	}
	if !b.Existed {
		t.Fatal("期望备份记录「原本存在」")
	}
	// 根 + shell + shell\open + shell\open\command
	if len(b.Keys) != 4 {
		t.Fatalf("期望备份 4 个键，实际 %d（%+v）", len(b.Keys), b.Keys)
	}

	if err := applyHijack(keyPath, ourExe); err != nil {
		t.Fatalf("applyHijack: %v", err)
	}
	cur, err := currentCommandAt(keyPath)
	if err != nil {
		t.Fatalf("改写后读 command：%v", err)
	}
	if !strings.Contains(cur, ourExe) || !strings.Contains(cur, CallbackFlag) {
		t.Fatalf("改写后 command 不符：%q", cur)
	}

	if err := restoreFrom(keyPath, b); err != nil {
		t.Fatalf("restoreFrom: %v", err)
	}
	cur, err = currentCommandAt(keyPath)
	if err != nil {
		t.Fatalf("恢复后读 command：%v", err)
	}
	if !strings.Contains(cur, "商汤小浣熊.exe") {
		t.Fatalf("恢复后 command 未还原：%q", cur)
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, keyPath, registry.QUERY_VALUE)
	if err != nil {
		t.Fatalf("恢复后根键缺失：%v", err)
	}
	defer k.Close()
	if v, _, err := k.GetStringValue(""); err != nil || v != "URL:"+ProtocolScheme {
		t.Fatalf("根键默认值未还原：%q err=%v", v, err)
	}
	if v, _, err := k.GetStringValue("URL Protocol"); err != nil || v != "" {
		t.Fatalf("URL Protocol 未还原：%q err=%v", v, err)
	}
}

// TestProtocolRestoreWhenKeyAbsent 覆盖「用户没装官方客户端」路径：
// 备份记 Existed=false，恢复时要把键整棵删掉，且重复恢复不报错。
func TestProtocolRestoreWhenKeyAbsent(t *testing.T) {
	keyPath := testKeyPath(t)
	cleanupKey(t, keyPath)
	const ourExe = `D:\tools\wild-work.exe`

	b, err := captureBackup(keyPath, ourExe)
	if err != nil {
		t.Fatalf("captureBackup: %v", err)
	}
	if b.Existed {
		t.Fatal("键不存在时 Existed 应为 false")
	}
	if err := applyHijack(keyPath, ourExe); err != nil {
		t.Fatalf("applyHijack: %v", err)
	}
	if _, err := currentCommandAt(keyPath); err != nil {
		t.Fatalf("改写后应能读到 command：%v", err)
	}
	if err := restoreFrom(keyPath, b); err != nil {
		t.Fatalf("restoreFrom: %v", err)
	}
	if _, err := registry.OpenKey(registry.CURRENT_USER, keyPath, registry.QUERY_VALUE); err == nil {
		t.Fatal("恢复后键应已删除")
	}
	if err := restoreFrom(keyPath, b); err != nil {
		t.Fatalf("重复 restoreFrom 应无错：%v", err)
	}
}

// TestShouldRestore 覆盖「是否该按备份回写」的判定：
// 只有当前命令仍指向我们时才恢复，被官方客户端重写后必须放手。
func TestShouldRestore(t *testing.T) {
	const ourExe = `D:\tools\wild-work.exe`
	cases := []struct {
		name    string
		current string
		exe     string
		want    bool
	}{
		{"仍是我们的", "\"" + ourExe + "\" " + CallbackFlag + " \"%1\"", ourExe, true},
		{"被客户端重写", `"C:\Program Files\raccoon-ai\商汤小浣熊.exe" "%1"`, ourExe, false},
		{"键被删（读不到）", "", ourExe, true},
		{"备份没记 exe", `"C:\whatever.exe" "%1"`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldRestore(tc.current, tc.exe); got != tc.want {
				t.Fatalf("shouldRestore(%q,%q) 期望 %v 实际 %v", tc.current, tc.exe, tc.want, got)
			}
		})
	}
}

// TestCommandFor 校验注入命令行的形状：路径带空格时必须加引号、%1 必须保留。
func TestCommandFor(t *testing.T) {
	got := commandFor(`D:\a b\wild-work.exe`)
	want := `"D:\a b\wild-work.exe" ` + CallbackFlag + ` "%1"`
	if got != want {
		t.Fatalf("commandFor 期望 %q 实际 %q", want, got)
	}
}
