package geoip

import (
	"strings"

	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/pytext"
)

// parseIPCandidate normalizes one forwarded value: surrounding whitespace
// and quotes are removed, "[v6]:port" and "v4:port" lose the port. It
// returns the original text of the address (not a canonical form).
func parseIPCandidate(raw string) (string, bool) {
	value := strings.Trim(pytext.Strip(raw), `"`)
	if value == "" || strings.ToLower(value) == "unknown" {
		return "", false
	}
	if strings.HasPrefix(value, "[") {
		if end := strings.Index(value, "]"); end >= 0 {
			value = value[1:end]
		}
	}
	if _, ok := ParseIP(value); ok {
		return value, true
	}
	if strings.Count(value, ":") == 1 && strings.Contains(value, ".") {
		host := value[:strings.LastIndex(value, ":")]
		if _, ok := ParseIP(host); ok {
			return host, true
		}
	}
	return "", false
}

// ClientIP picks the client address best-effort, without trusted-proxy
// semantics: the first valid entry of X-Forwarded-For, then X-Real-IP, then
// the connection's peer address. Header values must already be decoded as
// latin-1. It returns "" when nothing valid is found.
func ClientIP(forwardedFor, realIP, peer string) string {
	for _, item := range strings.Split(forwardedFor, ",") {
		if ip, ok := parseIPCandidate(item); ok {
			return ip
		}
	}
	if ip, ok := parseIPCandidate(realIP); ok {
		return ip
	}
	ip, _ := parseIPCandidate(peer)
	return ip
}
