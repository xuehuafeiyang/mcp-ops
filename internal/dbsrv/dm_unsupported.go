//go:build !linux && !windows

package dbsrv

// 达梦官方 Go 驱动仅提供 Linux/Windows 的加密库加载实现，
// 在 macOS 等其他平台无法编译，故此处不导入驱动，运行时返回友好错误。
const dmAvailable = false
