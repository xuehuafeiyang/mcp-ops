//go:build windows

package builtins

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var (
	shell32DLL = syscall.NewLazyDLL("shell32.dll")
	procShellExecuteW = shell32DLL.NewProc("ShellExecuteW")
)

// shellLaunchAvailable 报告当前平台是否支持 Shell 启动路径。
func shellLaunchAvailable() bool { return true }

// shellLaunchBrowser 用 ShellExecuteW 拉起浏览器进程。
//
// 为什么不走 os/exec（CreateProcess）：实测（2026-11）本机环境下，
// CreateProcess 直启的 Edge 147 会静默退出——exit 0、无 WER/事件日志、
// --enable-logging 无任何输出；同一命令行经 ShellExecute（资源管理器/
// PowerShell Start-Process 同路径）拉起则完全正常，且可稳定复现。
// 故浏览器固定走 Shell 启动 + 固定调试端口 + RemoteAllocator 附着。
func shellLaunchBrowser(exe string, args []string) error {
	verb, _ := syscall.UTF16PtrFromString("open")
	file, _ := syscall.UTF16PtrFromString(exe)
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = syscall.EscapeArg(a)
	}
	params, _ := syscall.UTF16PtrFromString(strings.Join(quoted, " "))
	ret, _, callErr := procShellExecuteW.Call(
		0,
		uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(file)),
		uintptr(unsafe.Pointer(params)),
		0,
		1, // SW_SHOWNORMAL：headed 调试模式下可见
	)
	if ret <= 32 {
		return fmt.Errorf("ShellExecuteW(%s) ret=%d err=%v", exe, ret, callErr)
	}
	return nil
}

// pickFreePort 申请一个可用的本机回环端口（有微小竞态，CDP 轮询失败时由调用方重试）。
func pickFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port, nil
}

// waitCDP 轮询 CDP /json/version 端点，返回浏览器的 webSocketDebuggerUrl。
func waitCDP(port int, timeout time.Duration) (string, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/json/version", port)
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			var info struct {
				WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
			}
			err = json.NewDecoder(resp.Body).Decode(&info)
			resp.Body.Close()
			if err == nil && info.WebSocketDebuggerURL != "" {
				return info.WebSocketDebuggerURL, nil
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return "", fmt.Errorf("等待浏览器 CDP 端口 %d 就绪超时", port)
}
