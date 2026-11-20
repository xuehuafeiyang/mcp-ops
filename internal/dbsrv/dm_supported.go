//go:build linux || windows

package dbsrv

// 达梦官方 Go 驱动（github.com/ganl/go-dm）的 security 包依赖平台特定的
// 动态库加载实现（Linux plugin / Windows LazyDLL），官方未提供 macOS 版本，
// 因此仅在 Linux/Windows 上导入并注册 driver name "dm"。
import _ "github.com/ganl/go-dm"

// dmAvailable 标记当前平台是否可用达梦驱动。
const dmAvailable = true
