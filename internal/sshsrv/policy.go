package sshsrv

import (
	"fmt"
	"regexp"
	"strings"

	"mcp-ops/internal/config"
)

// Policy 实现“白名单前缀 + 危险命令黑名单兜底”两级检查。
//
// 黑名单：对整条命令做模式检查，无论是否在白名单内命中，一律拒绝。
//
// 白名单：先按未被引号包裹的 shell 分隔符（; && || | 换行）把命令切段，
// 每一段都必须精确等于某个 allowed 项，或以 “allowed + 空格” 开头
// （token 边界匹配，避免 "docker ps" 放行 "docker psx"）。
// 逐段校验保证 "df -h; cat /etc/shadow" 不会因为以 df 开头而整条放行。
var dangerousPatterns = []string{
	"rm -rf", "rm -fr",
	"dd ", "dd if=", "dd of=",
	"mkfs", "mkfs.",
	"shutdown", "reboot", "halt", "poweroff",
	"init 0", "init 6",
	":(){", "fork bomb",
	"chmod -r 777 /", "chmod -R 777 /",
	"chown -r 777 /", "chown -R 777 /",
	// 只拦对块设备的直接写入；>/dev/null 这类正常重定向不拦
	">/dev/sd", "> /dev/sd", ">/dev/nvme", "> /dev/nvme",
	">/dev/mmcblk", "> /dev/mmcblk", ">/dev/mem", "> /dev/mem",
	">/dev/kmem", "> /dev/kmem", ">/dev/port", "> /dev/port",
	">/dev/disk", "> /dev/disk", "of=/dev/sd", "of=/dev/nvme",
	"kill -9 1", "kill -HUP 1",
	"curl | sh", "curl |bash", "wget | sh", "wget |bash",
	"shred",
}

// dangerousRegexps 补充子串匹配覆盖不到的“下载即执行”形态，
// 例如 "curl http://x/i.sh | sh"、"wget -qO- url | sudo bash"。
var dangerousRegexps = []*regexp.Regexp{
	regexp.MustCompile(`\b(curl|wget)\b[^|]*\|\s*(sudo\s+)?(ba|z|k|da)?sh\b`),
	regexp.MustCompile(`\b(echo|base64|xxd|printf)\b[^|]*\|\s*(sudo\s+)?(ba|z|k)?sh\b`),
	regexp.MustCompile(`\b(sh|bash)\s+<\s*\(`),
}

func CheckPolicy(host *config.SSHHost, cmd string) error {
	c := strings.TrimSpace(cmd)
	if c == "" {
		return fmt.Errorf("empty command")
	}

	// 第一级：危险命令黑名单（兜底，永远生效）
	lower := strings.ToLower(c)
	for _, pat := range dangerousPatterns {
		if strings.Contains(lower, strings.ToLower(pat)) {
			return fmt.Errorf("command blocked by dangerous-pattern policy: %q", pat)
		}
	}
	for _, re := range dangerousRegexps {
		if re.MatchString(lower) {
			return fmt.Errorf("command blocked by dangerous-pattern policy (download-and-execute): %s", re.String())
		}
	}

	// 第二级：白名单前缀（token 边界）
	if len(host.AllowedCommands) == 0 {
		return fmt.Errorf("host %q has empty allowed_commands; all commands denied", host.Name)
	}
	// 通配：allowed_commands 含 "*" 表示放行全部命令（危险命令黑名单仍在上方兜底）
	for _, allow := range host.AllowedCommands {
		if strings.TrimSpace(allow) == "*" {
			return nil
		}
	}
	for _, seg := range splitShellSegments(c) {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue // "a && b" 这类切分产生的空段
		}
		if !matchesAllowlist(seg, host.AllowedCommands) {
			return fmt.Errorf("command segment %q not in allowed_commands of host %q (allowed: %s)",
				seg, host.Name, strings.Join(host.AllowedCommands, ", "))
		}
	}
	return nil
}

func matchesAllowlist(seg string, allowed []string) bool {
	for _, allow := range allowed {
		a := strings.TrimSpace(allow)
		if a == "" {
			continue
		}
		if seg == a || strings.HasPrefix(seg, a+" ") {
			return true
		}
	}
	return false
}

// splitShellSegments 按未被引号包裹的 ; & | 与换行切分命令。
// 仅用于白名单逐段校验；不解析命令替换 $( )、反引号、变量展开等更深层语法，
// 这些仍需依赖黑名单与最小权限账号兜底。
func splitShellSegments(cmd string) []string {
	var segs []string
	var b strings.Builder
	var inSingle, inDouble bool
	for i := 0; i < len(cmd); i++ {
		ch := cmd[i]
		// 反斜杠转义：原样保留，跳过下一个字符，避免转义的引号被误判
		if ch == '\\' && !inSingle && i+1 < len(cmd) {
			b.WriteByte(ch)
			i++
			b.WriteByte(cmd[i])
			continue
		}
		switch {
		case ch == '\'' && !inDouble:
			inSingle = !inSingle
		case ch == '"' && !inSingle:
			inDouble = !inDouble
		}
		if !inSingle && !inDouble {
			switch ch {
			case ';', '|', '&', '\n':
				segs = append(segs, b.String())
				b.Reset()
				continue
			}
		}
		b.WriteByte(ch)
	}
	return append(segs, b.String())
}
