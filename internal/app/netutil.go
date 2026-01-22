package app

import (
	"net"
	"strings"
)

// isLoopbackListenAddr returns true if addr binds only to loopback.
// Examples that are NOT loopback-only: ":3210", "0.0.0.0:3210", "[::]:3210".
func isLoopbackListenAddr(addr string) bool {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return false
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// best-effort fallback
		return false
	}
	host = strings.TrimSpace(host)
	if host == "" {
		// ":port" binds all interfaces
		return false
	}
	// strip brackets for IPv6
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}
