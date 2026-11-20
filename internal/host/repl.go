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
	fmt.Println("命令: /tools 查看工具  /resources 资源清单  /prompts 提示词清单  /reset 清空会话  /quit 退出")

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
		if strings.HasPrefix(line, "/resources") {
			printResources(a, strings.TrimSpace(strings.TrimPrefix(line, "/resources")))
			continue
		}
		if strings.HasPrefix(line, "/prompts") {
			printPrompts(a, strings.TrimSpace(strings.TrimPrefix(line, "/prompts")))
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

// printResources 处理 /resources 命令。
//   /resources                 列出各 server 的资源清单
//   /resources <server> <uri>  读取指定资源内容
func printResources(a *Agent, rest string) {
	fields := strings.Fields(rest)
	if len(fields) >= 2 {
		uri := strings.Join(fields[1:], " ")
		rc, err := a.ReadResource(fields[0], uri)
		if err != nil {
			fmt.Printf("\x1b[31m❌ %v\x1b[0m\n\n", err)
			return
		}
		text := rc.Text
		if text == "" && rc.Blob != "" {
			text = fmt.Sprintf("[二进制内容 %s，%d 字符 base64]", rc.MimeType, len(rc.Blob))
		}
		fmt.Printf("%s\n\n", text)
		return
	}
	res := a.Resources()
	total := 0
	for server, list := range res {
		for _, r := range list {
			total++
			desc := r.Description
			if i := strings.IndexByte(desc, '\n'); i > 0 {
				desc = desc[:i]
			}
			if r := []rune(desc); len(r) > 50 {
				desc = string(r[:50]) + "..."
			}
			fmt.Printf("  %-10s %-45s [%s] %s\n", server, r.URI, r.Name, desc)
		}
	}
	if total == 0 {
		fmt.Println("（无资源；仅声明了 resources 能力的 server 会列出）")
	}
	fmt.Println()
}

// printPrompts 处理 /prompts 命令。
//   /prompts                 列出各 server 的提示词清单
//   /prompts <server> <name> 获取指定提示词内容
func printPrompts(a *Agent, rest string) {
	fields := strings.Fields(rest)
	if len(fields) >= 2 {
		pr, err := a.GetPrompt(fields[0], fields[1], nil)
		if err != nil {
			fmt.Printf("\x1b[31m❌ %v\x1b[0m\n\n", err)
			return
		}
		for _, m := range pr.Messages {
			fmt.Printf("[%s] %s\n", m.Role, m.Content.Text)
		}
		fmt.Println()
		return
	}
	prompts := a.Prompts()
	total := 0
	for server, list := range prompts {
		for _, p := range list {
			total++
			desc := p.Description
			if i := strings.IndexByte(desc, '\n'); i > 0 {
				desc = desc[:i]
			}
			if r := []rune(desc); len(r) > 50 {
				desc = string(r[:50]) + "..."
			}
			fmt.Printf("  %-10s %-40s %s\n", server, p.Name, desc)
		}
	}
	if total == 0 {
		fmt.Println("（无提示词；仅声明了 prompts 能力的 server 会列出）")
	}
	fmt.Println()
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
