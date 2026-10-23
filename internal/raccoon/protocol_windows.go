//go:build windows

// protocol_windows.go 协议回调接管的注册表实现（Windows）。
//
// 做法：登录期间把 HKCU\Software\Classes\office-raccoon 的 shell\open\command
// 临时改成 wild-work 自身，从而在网页授权完成后收到 office-raccoon://auth/callback?code=…
// 深链（Windows 会以 `"<exe>" --raccoon-callback "office-raccoon://…"` 唤起本进程）；
// 拿到授权码后立刻把注册表恢复原状。
//
// 实测原始值（VM 取证，2026-09-23）：
//
//	HKCU\Software\Classes\office-raccoon
//	    (Default)      REG_SZ  "URL:office-raccoon"
//	    URL Protocol   REG_SZ  ""
//	  \shell\open\command
//	    (Default)      REG_SZ  "\"C:\Program Files\raccoon-ai\商汤小浣熊.exe\" \"%1\""
//
// 三条安全约束（务必保持）：
//  1. 先备份落盘、再改写；备份写不成功就绝不碰注册表。
//  2. 恢复时先校验「当前值仍是我们写入的」——若已被官方客户端重写（它在启动时会重新注册协议），
//     就只清备份不动注册表，否则会把注册表改回过期路径。
//  3. 恢复是幂等的：重复调用、备份已删、键本就不存在，都不得报错。
package raccoon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// callbackFlag 内部别名（常量定义在 protocol.go，跨平台可见，入口层用它识别参数）。
const callbackFlag = CallbackFlag

// regValue 单个注册表值的可序列化形态（按 Type 选用对应字段）。
type regValue struct {
	Name string   `json:"name"`
	Type uint32   `json:"type"`
	Str  string   `json:"str,omitempty"`
	Num  uint64   `json:"num,omitempty"`
	Strv []string `json:"strv,omitempty"`
	Bin  []byte   `json:"bin,omitempty"`
}

// regKey 一个键及其值；多个键以**前序**（父先于子）扁平存放，恢复时按序重建即可。
type regKey struct {
	Path   string     `json:"path"`
	Values []regValue `json:"values"`
}

// protocolBackup 改写前的注册表快照。
type protocolBackup struct {
	Existed   bool     `json:"existed"`
	Keys      []regKey `json:"keys"`
	ExePath   string   `json:"exePath"`
	CreatedAt int64    `json:"createdAt"`
}

// HijackProtocol 备份并把协议注册表指向本工具。exePath 为 wild-work 自身可执行文件路径。
func HijackProtocol(exePath, backupFP string) error {
	exePath = strings.TrimSpace(exePath)
	if exePath == "" {
		return errors.New("无法确定自身可执行文件路径，小浣熊授权登录不可用")
	}
	b, err := captureBackup(ProtocolKeyPath, exePath)
	if err != nil {
		return err
	}
	// 先落盘备份，再动注册表：万一后面崩了，下次启动能靠它自愈。
	if err := writeBackup(backupFP, b); err != nil {
		return err
	}
	if err := applyHijack(ProtocolKeyPath, exePath); err != nil {
		// 改写失败：立即回滚，不留半截状态。
		_ = restoreFrom(ProtocolKeyPath, b)
		_ = os.Remove(backupFP)
		return err
	}
	return nil
}

// RestoreProtocol 按备份恢复注册表，并删除备份文件。
// 返回值 restored 表示是否真的改动了注册表（false = 已被官方客户端自行修好，或本就无备份）。
func RestoreProtocol(backupFP string) (bool, error) {
	b, err := readBackup(backupFP)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	cur, cerr := currentCommandAt(ProtocolKeyPath)
	if cerr == nil && !shouldRestore(cur, b.ExePath) {
		// 当前命令已不指向我们：多半是登录期间官方客户端启动并重写了协议注册。
		// 它已自行恢复，此时按备份回写只会把注册表改回过期路径 → 只清备份。
		_ = os.Remove(backupFP)
		return false, nil
	}
	if err := restoreFrom(ProtocolKeyPath, b); err != nil {
		return false, err
	}
	if err := os.Remove(backupFP); err != nil && !os.IsNotExist(err) {
		return true, err
	}
	return true, nil
}

// IsHijackedBy 当前协议命令是否指向给定 exe（用于诊断与面板展示）。
func IsHijackedBy(exePath string) bool {
	exePath = strings.TrimSpace(exePath)
	if exePath == "" {
		return false
	}
	cur, err := currentCommandAt(ProtocolKeyPath)
	if err != nil {
		return false
	}
	return strings.Contains(cur, exePath)
}

// HasPendingHijack 是否存在未清理的改设备份（= 上次登录没走到恢复，需要启动自愈）。
func HasPendingHijack(stateDir string) bool {
	_, err := os.Stat(BackupPath(stateDir))
	return err == nil
}

// CurrentProtocolCommand 读当前协议命令（面板诊断用；失败返回空串）。
func CurrentProtocolCommand() string {
	cur, err := currentCommandAt(ProtocolKeyPath)
	if err != nil {
		return ""
	}
	return cur
}

// ---------------------------------------------------------------------------
// 内部实现
// ---------------------------------------------------------------------------

// commandFor 生成改写用的命令行。%1 由 Windows 替换为深链原文（含引号）。
func commandFor(exePath string) string {
	return "\"" + exePath + "\" " + callbackFlag + " \"%1\""
}

// shouldRestore 判断是否该按备份回写注册表。
//
// 只有「当前命令仍指向我们写入的 exe」时才恢复：若已被别人改写（典型场景是登录期间
// 官方客户端启动并重新注册了协议），它已经自行恢复好了，此时再按备份回写反而会把
// 注册表改回过期路径。读不到当前值（键被删）或备份没记 exe 时，按备份恢复更安全。
func shouldRestore(current, exePath string) bool {
	if strings.TrimSpace(exePath) == "" {
		return true
	}
	if strings.TrimSpace(current) == "" {
		return true
	}
	return strings.Contains(current, exePath)
}

// currentCommandAt 读指定协议键下处理器当前命令（keyPath 参数化以便测试用临时键）。
func currentCommandAt(keyPath string) (string, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		keyPath+`\shell\open\command`, registry.QUERY_VALUE)
	if err != nil {
		return "", err
	}
	defer k.Close()
	s, _, err := k.GetStringValue("")
	return s, err
}

// captureBackup 递归快照协议键（含子键与全部值）。
func captureBackup(keyPath, exePath string) (protocolBackup, error) {
	b := protocolBackup{ExePath: exePath, CreatedAt: time.Now().Unix()}
	k, err := registry.OpenKey(registry.CURRENT_USER, keyPath,
		registry.QUERY_VALUE|registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			// 用户没装官方客户端：记「原本不存在」，恢复时删干净即可。
			return b, nil
		}
		return b, fmt.Errorf("读取协议注册表失败：%w", err)
	}
	_ = k.Close()
	b.Existed = true
	var keys []regKey
	if err := walkKey(keyPath, "", &keys); err != nil {
		return b, fmt.Errorf("备份协议注册表失败：%w", err)
	}
	b.Keys = keys
	return b, nil
}

// walkKey 前序遍历键树，把每个键（含其值）追加到 out。
func walkKey(keyPath, rel string, out *[]regKey) error {
	full := keyPath
	if rel != "" {
		full += `\` + rel
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, full,
		registry.QUERY_VALUE|registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return err
	}
	defer k.Close()
	vals, err := readValues(k)
	if err != nil {
		return err
	}
	*out = append(*out, regKey{Path: rel, Values: vals})
	subs, err := k.ReadSubKeyNames(-1)
	if err != nil {
		return err
	}
	for _, s := range subs {
		child := s
		if rel != "" {
			child = rel + `\` + s
		}
		if err := walkKey(keyPath, child, out); err != nil {
			return err
		}
	}
	return nil
}

// readValues 读出键下全部值（类型保真）。
func readValues(k registry.Key) ([]regValue, error) {
	names, err := k.ReadValueNames(-1)
	if err != nil {
		return nil, err
	}
	out := make([]regValue, 0, len(names))
	for _, name := range names {
		_, t, err := k.GetValue(name, nil)
		if err != nil {
			continue
		}
		v := regValue{Name: name, Type: t}
		switch t {
		case registry.SZ, registry.LINK:
			if s, _, err := k.GetStringValue(name); err == nil {
				v.Str = s
			}
		case registry.EXPAND_SZ:
			// registry 包没有 GetExpandStringValue（只有 Set 侧有），
			// 且 GetStringValue 对非 SZ 类型会返回 ErrTypeMismatch → 自行按 UTF-16 解。
			v.Str = readRawString(k, name)
		case registry.DWORD, registry.DWORD_BIG_ENDIAN, registry.QWORD:
			if n, _, err := k.GetIntegerValue(name); err == nil {
				v.Num = n
			}
		case registry.MULTI_SZ:
			if ss, _, err := k.GetStringsValue(name); err == nil {
				v.Strv = ss
			}
		default:
			if b, _, err := k.GetBinaryValue(name); err == nil {
				v.Bin = b
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// readRawString 读 REG_EXPAND_SZ 的原始内容（按 UTF-16LE 解码，不做环境变量展开）。
func readRawString(k registry.Key, name string) string {
	n, _, err := k.GetValue(name, nil)
	if err != nil || n <= 0 {
		return ""
	}
	buf := make([]byte, n)
	n, _, err = k.GetValue(name, buf)
	if err != nil || n <= 0 {
		return ""
	}
	u16 := make([]uint16, 0, n/2)
	for i := 0; i+1 < n; i += 2 {
		u16 = append(u16, uint16(buf[i])|uint16(buf[i+1])<<8)
	}
	return windows.UTF16ToString(u16)
}

// writeValue 按类型写回一个值。
func writeValue(k registry.Key, v regValue) error {
	switch v.Type {
	case registry.SZ, registry.LINK:
		return k.SetStringValue(v.Name, v.Str)
	case registry.EXPAND_SZ:
		return k.SetExpandStringValue(v.Name, v.Str)
	case registry.DWORD, registry.DWORD_BIG_ENDIAN:
		return k.SetDWordValue(v.Name, uint32(v.Num))
	case registry.QWORD:
		return k.SetQWordValue(v.Name, v.Num)
	case registry.MULTI_SZ:
		return k.SetStringsValue(v.Name, v.Strv)
	default:
		return k.SetBinaryValue(v.Name, v.Bin)
	}
}

// applyHijack 把协议命令改成指向 wild-work。
func applyHijack(keyPath, exePath string) error {
	root, _, err := registry.CreateKey(registry.CURRENT_USER, keyPath, registry.ALL_ACCESS)
	if err != nil {
		return fmt.Errorf("创建协议键失败：%w", err)
	}
	_ = root.SetStringValue("", "URL:"+ProtocolScheme)
	_ = root.SetStringValue("URL Protocol", "")
	_ = root.Close()

	cmdKey, _, err := registry.CreateKey(registry.CURRENT_USER,
		keyPath+`\shell\open\command`, registry.ALL_ACCESS)
	if err != nil {
		return fmt.Errorf("创建协议命令键失败：%w", err)
	}
	defer cmdKey.Close()
	if err := cmdKey.SetStringValue("", commandFor(exePath)); err != nil {
		return fmt.Errorf("写入协议命令失败：%w", err)
	}
	return nil
}

// restoreFrom 按快照重建注册表：先整棵删掉，再按前序列表重建。
func restoreFrom(keyPath string, b protocolBackup) error {
	if err := deleteTree(keyPath, ""); err != nil {
		return fmt.Errorf("清理协议键失败：%w", err)
	}
	if !b.Existed {
		return nil
	}
	for _, k := range b.Keys {
		full := keyPath
		if k.Path != "" {
			full += `\` + k.Path
		}
		rk, _, err := registry.CreateKey(registry.CURRENT_USER, full, registry.ALL_ACCESS)
		if err != nil {
			return fmt.Errorf("重建协议键 %q 失败：%w", k.Path, err)
		}
		for _, v := range k.Values {
			if err := writeValue(rk, v); err != nil {
				_ = rk.Close()
				return fmt.Errorf("恢复协议键 %q 的值 %q 失败：%w", k.Path, v.Name, err)
			}
		}
		_ = rk.Close()
	}
	return nil
}

// deleteTree 后序删除键树（registry.DeleteKey 只能删无子键的键）。
func deleteTree(keyPath, rel string) error {
	full := keyPath
	if rel != "" {
		full += `\` + rel
	}
	if k, err := registry.OpenKey(registry.CURRENT_USER, full, registry.ENUMERATE_SUB_KEYS); err == nil {
		subs, _ := k.ReadSubKeyNames(-1)
		_ = k.Close()
		for _, s := range subs {
			child := s
			if rel != "" {
				child = rel + `\` + s
			}
			if err := deleteTree(keyPath, child); err != nil {
				return err
			}
		}
	}
	if err := registry.DeleteKey(registry.CURRENT_USER, full); err != nil &&
		!errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

// writeBackup 原子写备份文件。
func writeBackup(fp string, b protocolBackup) error {
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if dir := filepath.Dir(fp); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("创建备份目录失败：%w", err)
		}
	}
	tmp := fp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("写入注册表备份失败：%w", err)
	}
	if err := os.Rename(tmp, fp); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("落盘注册表备份失败：%w", err)
	}
	return nil
}

// readBackup 读备份文件。
func readBackup(fp string) (protocolBackup, error) {
	var b protocolBackup
	raw, err := os.ReadFile(fp)
	if err != nil {
		return b, err
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return b, fmt.Errorf("注册表备份损坏（%s）：%w", fp, err)
	}
	return b, nil
}
