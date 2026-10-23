//go:build !windows

// protocol_other.go 非 Windows 平台的退化实现。
//
// 协议回调接管依赖 HKCU\Software\Classes 的 URL Protocol 注册（Windows 专有机制），
// 且两个官方客户端本身只有 Windows 版，故其余平台一律按「不支持」处理——
// 调用方拿到 ErrProtocolUnsupported 后应引导用户改用「从本机客户端导入」。
package raccoon

// HijackProtocol 非 Windows 恒失败。
func HijackProtocol(exePath, backupFP string) error { return ErrProtocolUnsupported }

// RestoreProtocol 非 Windows 无注册表可改：恒成功且不改动任何东西。
func RestoreProtocol(backupFP string) (bool, error) { return false, nil }

// IsHijackedBy 非 Windows 恒 false。
func IsHijackedBy(exePath string) bool { return false }

// HasPendingHijack 非 Windows 恒 false。
func HasPendingHijack(stateDir string) bool { return false }

// CurrentProtocolCommand 非 Windows 恒空串。
func CurrentProtocolCommand() string { return "" }
