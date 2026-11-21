//go:build !windows

package builtins

import (
	"errors"
	"time"
)

// shellLaunchAvailable 报告当前平台是否支持 Shell 启动路径。
func shellLaunchAvailable() bool { return false }

// shellLaunchBrowser 仅 Windows 实现（ShellExecuteW）；其他平台走 ExecAllocator 回退路径。
func shellLaunchBrowser(exe string, args []string) error {
	return errors.New("shell launch 仅支持 Windows")
}

// pickFreePort 申请一个可用的本机回环端口。
func pickFreePort() (int, error) {
	return 0, errors.New("仅 Windows 实现")
}

// waitCDP 轮询 CDP /json/version 端点。
func waitCDP(port int, timeout time.Duration) (string, error) {
	return "", errors.New("仅 Windows 实现")
}
