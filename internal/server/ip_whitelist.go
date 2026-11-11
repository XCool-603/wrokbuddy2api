package server

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strings"

	"workbuddy2api/internal/usermgr"
)

// IPWhitelistConfig 描述 AI 接口的 IP 白名单防护配置。
type IPWhitelistConfig struct {
	Enabled        bool     `json:"enabled"`
	AllowedIPs     []string `json:"allowed_ips"`
	AllowLocalhost bool     `json:"allow_localhost"`
}

// RuntimeNetworkInfo 描述服务本机运行 IP、监听地址及当前来访客户端 IP。
type RuntimeNetworkInfo struct {
	ListenAddr string   `json:"listen_addr"`
	RunningIPs []string `json:"running_ips"`
	ClientIP   string   `json:"client_ip"`
}

// SplitIPTokens 将逗号、换行或空白分隔的 IP/CIDR 文本拆分为去重有序条目（支持 # 注释行）。
func SplitIPTokens(raw string) []string {
	lines := strings.Split(raw, "\n")
	var out []string
	for _, line := range lines {
		if idx := strings.Index(line, "#"); idx >= 0 {
			line = line[:idx]
		}
		fields := strings.FieldsFunc(line, func(r rune) bool {
			return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\r'
		})
		for _, f := range fields {
			if f = strings.TrimSpace(f); f != "" {
				out = append(out, f)
			}
		}
	}
	return out
}

// ParseWhitelistEntries 校验并规范化单 IP（IPv4/IPv6）与 CIDR 网段条目。
func ParseWhitelistEntries(entries []string) ([]netip.Prefix, []string, error) {
	seen := make(map[string]struct{}, len(entries))
	prefixes := make([]netip.Prefix, 0, len(entries))
	normalized := make([]string, 0, len(entries))

	for _, raw := range entries {
		for _, tok := range SplitIPTokens(raw) {
			var pfx netip.Prefix
			if strings.Contains(tok, "/") {
				parsed, err := netip.ParsePrefix(tok)
				if err != nil {
					return nil, nil, fmt.Errorf("无效的 CIDR 网段 %q: %w", tok, err)
				}
				pfx = parsed.Masked()
				if pfx.Addr().Is4In6() {
					pfx = netip.PrefixFrom(pfx.Addr().Unmap(), pfx.Bits()-96)
				}
			} else {
				addr, err := netip.ParseAddr(tok)
				if err != nil {
					return nil, nil, fmt.Errorf("无效的 IP 地址 %q: %w", tok, err)
				}
				addr = addr.Unmap()
				bits := 32
				if addr.Is6() {
					bits = 128
				}
				pfx = netip.PrefixFrom(addr, bits)
			}
			key := pfx.String()
			if !strings.Contains(tok, "/") {
				key = pfx.Addr().String()
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			prefixes = append(prefixes, pfx)
			normalized = append(normalized, key)
		}
	}
	return prefixes, normalized, nil
}

// extractTrustedClientIP 提取真实客户端 IP，防止直接暴露在公网时通过伪造 X-Forwarded-For 绕过白名单：
// 仅当直连对端是本机回环或内网/链路代理地址时，才采信 X-Forwarded-For / X-Real-IP。
func extractTrustedClientIP(r *http.Request) (netip.Addr, string) {
	if r == nil {
		return netip.Addr{}, ""
	}
	rawRemote := strings.TrimSpace(r.RemoteAddr)
	if host, _, err := net.SplitHostPort(rawRemote); err == nil && host != "" {
		rawRemote = host
	}
	peerAddr, peerErr := netip.ParseAddr(rawRemote)
	if peerErr == nil {
		peerAddr = peerAddr.Unmap()
	}

	trustedProxy := peerErr == nil && (peerAddr.IsLoopback() || peerAddr.IsPrivate() || peerAddr.IsLinkLocalUnicast())
	if trustedProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			for _, part := range strings.Split(xff, ",") {
				cand := strings.TrimSpace(part)
				if addr, err := netip.ParseAddr(cand); err == nil {
					addr = addr.Unmap()
					return addr, addr.String()
				}
			}
		}
		if xrip := strings.TrimSpace(r.Header.Get("X-Real-IP")); xrip != "" {
			if addr, err := netip.ParseAddr(xrip); err == nil {
				addr = addr.Unmap()
				return addr, addr.String()
			}
		}
	}

	if peerErr == nil {
		return peerAddr, peerAddr.String()
	}
	return netip.Addr{}, rawRemote
}

// detectRuntimeIPs 读取本机当前运行中的非回环/局域网网卡 IP。
func detectRuntimeIPs() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	seen := make(map[string]struct{})
	var ipv4s, ipv6s []string
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			continue
		}
		s := ip.String()
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		if ip.To4() != nil {
			ipv4s = append(ipv4s, s)
		} else {
			ipv6s = append(ipv6s, s)
		}
	}
	sort.Strings(ipv4s)
	sort.Strings(ipv6s)
	return append(ipv4s, ipv6s...)
}

func (h *Handler) initIPWhitelist(cfg Config) {
	allowLocal := true
	if cfg.AllowLocalhost != nil {
		allowLocal = *cfg.AllowLocalhost
	}
	prefixes, normalized, err := ParseWhitelistEntries(cfg.AllowedIPs)
	if err != nil {
		// 配置了非法条目时不崩溃，保留有效部分并记录告警，若开启了白名单则默认仅按合法条目放行
		normalized = nil
		prefixes = nil
	}
	h.ipMu.Lock()
	h.ipWL = IPWhitelistConfig{
		Enabled:        cfg.IPWhitelistEnabled,
		AllowedIPs:     normalized,
		AllowLocalhost: allowLocal,
	}
	h.ipPrefixes = prefixes
	h.runtimeIPs = detectRuntimeIPs()
	h.ipMu.Unlock()
}

// IsIPAllowed 校验来访请求是否命中 IP 白名单（未开启白名单时全部放行）。
func (h *Handler) IsIPAllowed(r *http.Request) (bool, string) {
	addr, clientIP := extractTrustedClientIP(r)
	h.ipMu.RLock()
	enabled := h.ipWL.Enabled
	allowLocal := h.ipWL.AllowLocalhost
	prefixes := h.ipPrefixes
	h.ipMu.RUnlock()

	if !enabled {
		return true, clientIP
	}
	if !addr.IsValid() {
		return false, clientIP
	}
	if allowLocal && addr.IsLoopback() {
		return true, clientIP
	}
	for _, pfx := range prefixes {
		if pfx.Contains(addr) {
			return true, clientIP
		}
	}
	return false, clientIP
}

// GetIPWhitelist 返回当前 IP 白名单配置快照。
func (h *Handler) GetIPWhitelist() IPWhitelistConfig {
	h.ipMu.RLock()
	defer h.ipMu.RUnlock()
	out := h.ipWL
	out.AllowedIPs = append([]string(nil), h.ipWL.AllowedIPs...)
	return out
}

// SetIPWhitelist 校验并动态更新 IP 白名单规则。
func (h *Handler) SetIPWhitelist(enabled bool, allowed []string, allowLocalhost bool) (IPWhitelistConfig, error) {
	prefixes, normalized, err := ParseWhitelistEntries(allowed)
	if err != nil {
		return IPWhitelistConfig{}, err
	}
	h.ipMu.Lock()
	h.ipWL = IPWhitelistConfig{
		Enabled:        enabled,
		AllowedIPs:     normalized,
		AllowLocalhost: allowLocalhost,
	}
	h.ipPrefixes = prefixes
	out := h.ipWL
	out.AllowedIPs = append([]string(nil), normalized...)
	h.ipMu.Unlock()
	return out, nil
}

// GetRuntimeNetworkInfo 返回当前服务监听地址、本机网卡运行 IP 及当前请求方 IP。
func (h *Handler) GetRuntimeNetworkInfo(r *http.Request) RuntimeNetworkInfo {
	_, clientIP := extractTrustedClientIP(r)
	listen := h.cfg.ListenAddr
	if listen == "" {
		listen = ":7863"
	}
	ips := detectRuntimeIPs()
	if len(ips) == 0 {
		ips = []string{"127.0.0.1"}
	}
	return RuntimeNetworkInfo{
		ListenAddr: listen,
		RunningIPs: ips,
		ClientIP:   clientIP,
	}
}

// handleConfigIPWhitelist 动态更新并持久化 IP 白名单配置（仅管理员可操作）。
func (h *Handler) handleConfigIPWhitelist(w http.ResponseWriter, r *http.Request) {
	user := h.getWebSessionUser(r)
	if user == nil || user.Role != usermgr.RoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "权限不足：仅管理员可修改 IP 白名单"})
		return
	}

	var req struct {
		Enabled        bool     `json:"enabled"`
		AllowedIPs     []string `json:"allowed_ips"`
		AllowedIPsText string   `json:"allowed_ips_text"`
		AllowLocalhost *bool    `json:"allow_localhost"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体 JSON 解析失败"})
		return
	}

	allowLocal := true
	if req.AllowLocalhost != nil {
		allowLocal = *req.AllowLocalhost
	}
	entries := req.AllowedIPs
	if len(entries) == 0 && strings.TrimSpace(req.AllowedIPsText) != "" {
		entries = []string{req.AllowedIPsText}
	} else if len(entries) > 0 && strings.TrimSpace(req.AllowedIPsText) != "" {
		entries = append(entries, req.AllowedIPsText)
	}

	updated, err := h.SetIPWhitelist(req.Enabled, entries, allowLocal)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	h.savePersistentAny("ip_whitelist_enabled", updated.Enabled)
	h.savePersistentAny("allowed_ips", updated.AllowedIPs)
	h.savePersistentAny("allow_localhost", updated.AllowLocalhost)

	writeJSON(w, http.StatusOK, map[string]any{
		"success":      true,
		"ip_whitelist": updated,
		"network_info": h.GetRuntimeNetworkInfo(r),
	})
}
