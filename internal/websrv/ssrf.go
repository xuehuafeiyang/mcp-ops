package websrv

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"syscall"
)

// 标准库 IsPrivate/IsLoopback 未覆盖、但对 SSRF 同样危险的特殊用途网段。
var extraBlocked = mustCIDRs(
	"0.0.0.0/8",       // 本网络
	"100.64.0.0/10",   // CGNAT，云厂商内网常用
	"192.0.0.0/24",    // IETF 协议分配
	"192.0.2.0/24",    // TEST-NET-1
	"198.18.0.0/15",   // 网络基准测试
	"198.51.100.0/24", // TEST-NET-2
	"203.0.113.0/24",  // TEST-NET-3
	"240.0.0.0/4",     // 保留
	"2001:db8::/32",   // IPv6 文档网段
	"64:ff9b::/96",    // NAT64
)

func mustCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic("websrv: bad CIDR " + c)
		}
		out = append(out, n)
	}
	return out
}

// blockedHostnames 是即使不解析也必须拒绝的主机名（云元数据服务等）。
var blockedHostnames = []string{
	"localhost",
	"metadata",
	"metadata.google.internal",
	"metadata.goog",
	"instance-data",
	"instance-data.ec2.internal",
}

// isBlockedIP 判断 IP 是否属于不可抓取的范围：
// 环回、私网、链路本地（含 169.254.169.254 云元数据）、组播、未指定等。
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	for _, n := range extraBlocked {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func isBlockedHostname(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	for _, b := range blockedHostnames {
		if h == b {
			return true
		}
	}
	return strings.HasSuffix(h, ".localhost")
}

// checkURL 做请求前校验：协议白名单 + 主机名黑名单 + DNS 解析后的 IP 检查。
// 这是第一道防线，给用户更明确的错误；真正的兜底是 dialControl（见下）。
func checkURL(ctx context.Context, u *url.URL, allowPrivate bool) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("unsupported scheme %q: only http and https are allowed", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("URL has no host")
	}
	if allowPrivate {
		return nil
	}
	if isBlockedHostname(host) {
		return fmt.Errorf("refusing to fetch non-public host %q (SSRF protection); set web_fetch.allow_private to override", host)
	}
	// 字面量 IP 直接判断，避免无谓解析
	if ip := net.ParseIP(host); ip != nil {
		if isBlockedIP(ip) {
			return fmt.Errorf("refusing to fetch non-public address %s (SSRF protection); set web_fetch.allow_private to override", ip)
		}
		return nil
	}
	// 域名：解析后逐个检查。注意这里与真正连接之间仍存在理论上的 DNS rebinding 窗口，
	// 因此 dialControl 会在建立连接时对实际 IP 再次校验。
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("resolve %q: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("resolve %q: no addresses", host)
	}
	for _, a := range ips {
		if isBlockedIP(a.IP) {
			return fmt.Errorf("refusing to fetch %q: resolves to non-public address %s (SSRF protection)", host, a.IP)
		}
	}
	return nil
}

// dialControl 在 TCP 连接建立前拿到“真正要连的 IP”，
// 从而堵住 DNS rebinding：校验与连接之间不再有机会换 IP。
// 这一层比官方 mcp-server-fetch 更严（它在 README 中明确承认该窗口无法防住）。
func dialControl(allowPrivate bool) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, _ syscall.RawConn) error {
		if allowPrivate {
			return nil
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return fmt.Errorf("unexpected dial address %q: %w", address, err)
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return fmt.Errorf("unexpected dial address %q: not an IP", address)
		}
		if isBlockedIP(ip) {
			return fmt.Errorf("blocked connection to non-public address %s (SSRF protection)", ip)
		}
		return nil
	}
}
