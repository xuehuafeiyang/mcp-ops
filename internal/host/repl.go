package host

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// RunREPL 启动终端交互循环
func RunREPL(a *Agent, banner string) {
	fmt.Println(banner)
	fmt.Println("命令: /tools 查看工具  /reset 清空会话  /quit 退出")

	session := NewSession(a)
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)

	for {
		fmt.Printf("%s> ", "\x1b[36mmcp-ops\x1b[0m")
		if !sc.Scan() {
			fmt.Println()
			return
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		switch line {
		case "/quit", "/exit", "/q":
			return
		case "/reset":
			session.Reset()
			fmt.Println("✅ 会话已清空")
			continue
		case "/tools":
			printTools(a)
			continue
		case "/history":
			fmt.Printf("当前会话 %d 条消息\n\n", session.HistoryLen())
			continue
		}

		fmt.Println()
		text, err := session.Turn(line, func(p string) { fmt.Println(p) })
		if err != nil {
			fmt.Printf("\x1b[31m❌ %v\x1b[0m\n\n", err)
			continue
		}
		fmt.Printf("\n%s\n\n", text)
	}
}

func printTools(a *Agent) {
	fmt.Printf("可用工具 (%d):\n", len(a.Tools()))
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, t := range a.tools {
		server := a.routes[t.Name]
		desc := t.Description
		if i := strings.IndexByte(desc, '\n'); i > 0 {
			desc = desc[:i]
		}
		r := []rune(desc)
		if len(r) > 60 {
			desc = string(r[:60]) + "..."
		}
		fmt.Printf("  %-28s [%s] %s\n", t.Name, server, desc)
	}
	fmt.Println()
}
