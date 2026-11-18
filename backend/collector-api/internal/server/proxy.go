package server

import (
	"net/netip"
	"strconv"
	"strings"

	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/geoip"
	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/pytext"
)

// trustedProxies reproduces uvicorn's ProxyHeadersMiddleware, which the
// Python image ran with its defaults (proxy headers on, FORWARDED_ALLOW_IPS
// defaulting to 127.0.0.1): when the peer is trusted, X-Forwarded-Proto sets
// the scheme and X-Forwarded-For the client host.
type trustedProxies struct {
	all      bool
	literals map[string]bool
	hosts    []scopedAddr
	networks []netip.Prefix
}

type scopedAddr struct {
	addr  netip.Addr
	scope string
}

// parseScoped is ipaddress.ip_address keeping the IPv6 scope (address
// equality in Python includes it).
func parseScoped(s string) (scopedAddr, bool) {
	addr, ok := geoip.ParseIP(s)
	if !ok {
		return scopedAddr{}, false
	}
	scope := ""
	if addr.Is6() {
		_, scope, _ = strings.Cut(s, "%")
	}
	return scopedAddr{addr: addr, scope: scope}, true
}

func newTrustedProxies(value string) *trustedProxies {
	t := &trustedProxies{all: value == "*", literals: map[string]bool{}}
	if t.all {
		return t
	}
	for _, host := range parseRawHosts(value) {
		if strings.Contains(host, "/") {
			if prefix, ok := parseNetwork(host); ok {
				t.networks = append(t.networks, prefix)
			} else {
				t.literals[host] = true
			}
			continue
		}
		if a, ok := parseScoped(host); ok {
			t.hosts = append(t.hosts, a)
		} else {
			t.literals[host] = true
		}
	}
	return t
}

// parseNetwork accepts "address/prefixlen" with no host bits set, like
// ipaddress.ip_network(strict=True) does for numeric prefix lengths.
func parseNetwork(s string) (netip.Prefix, bool) {
	addrText, bitsText, _ := strings.Cut(s, "/")
	addr, ok := geoip.ParseIP(addrText)
	if !ok || strings.Contains(addrText, "%") || bitsText == "" {
		return netip.Prefix{}, false
	}
	for i := 0; i < len(bitsText); i++ {
		if bitsText[i] < '0' || bitsText[i] > '9' {
			return netip.Prefix{}, false
		}
	}
	bits, err := strconv.Atoi(bitsText)
	if err != nil || bits > addr.BitLen() {
		return netip.Prefix{}, false
	}
	prefix := netip.PrefixFrom(addr, bits)
	if prefix.Masked() != prefix {
		return netip.Prefix{}, false
	}
	return prefix, true
}

func parseRawHosts(value string) []string {
	items := strings.Split(value, ",")
	for i, item := range items {
		items[i] = pytext.Strip(item)
	}
	return items
}

func (t *trustedProxies) contains(host string) bool {
	if t.all {
		return true
	}
	if host == "" {
		return false
	}
	a, ok := parseScoped(host)
	if !ok {
		return t.literals[host]
	}
	for _, h := range t.hosts {
		if h == a {
			return true
		}
	}
	for _, n := range t.networks {
		if n.Addr().Is4() == a.addr.Is4() && n.Contains(a.addr) {
			return true
		}
	}
	return false
}

// clientHost picks the client host from an X-Forwarded-For value: the
// right-most untrusted entry, else the first one.
func (t *trustedProxies) clientHost(forwardedFor string) string {
	hosts := parseRawHosts(forwardedFor)
	if t.all {
		return hosts[0]
	}
	for i := len(hosts) - 1; i >= 0; i-- {
		if !t.contains(hosts[i]) {
			return hosts[i]
		}
	}
	return hosts[0]
}
