package settings

import (
	"errors"
	"net/netip"
	"strconv"
	"strings"
)

// ErrInvalidIPv6URL mirrors urllib.parse raising ValueError for unbalanced brackets.
var ErrInvalidIPv6URL = errors.New("Invalid IPv6 URL")

// RedactedURL keeps only scheme://host[:port] of a URL for logging, dropping
// credentials, path and query, with the same parsing rules as Python's urlsplit.
func RedactedURL(raw string) (string, error) {
	u := strings.TrimLeftFunc(raw, func(r rune) bool { return r <= ' ' })
	for _, unsafe := range []string{"\t", "\r", "\n"} {
		u = strings.ReplaceAll(u, unsafe, "")
	}
	scheme := ""
	if i := strings.IndexByte(u, ':'); i > 0 && isASCIIAlpha(u[0]) && strings.Trim(u[:i], schemeChars) == "" {
		scheme, u = strings.ToLower(u[:i]), u[i+1:]
	}
	netloc := ""
	if strings.HasPrefix(u, "//") {
		netloc = u[2:]
		if i := strings.IndexAny(netloc, "/?#"); i >= 0 {
			netloc = netloc[:i]
		}
		open, closed := strings.Contains(netloc, "["), strings.Contains(netloc, "]")
		if open != closed {
			return "", ErrInvalidIPv6URL
		}
		if open {
			_, after, _ := strings.Cut(netloc, "[")
			bracketed, _, _ := strings.Cut(after, "]")
			if !validBracketedHost(bracketed) {
				return "", ErrInvalidIPv6URL
			}
			hostPort := netloc[strings.LastIndexByte(netloc, '@')+1:]
			if before, inside, ok := strings.Cut(hostPort, "["); ok {
				_, rest, _ := strings.Cut(inside, "]")
				if before != "" || (rest != "" && rest[0] != ':') {
					return "", ErrInvalidIPv6URL
				}
			}
		}
	}

	hostinfo := netloc
	if i := strings.LastIndexByte(hostinfo, '@'); i >= 0 {
		hostinfo = hostinfo[i+1:]
	}
	var host, port string
	if _, bracketed, ok := strings.Cut(hostinfo, "["); ok {
		var rest string
		host, rest, _ = strings.Cut(bracketed, "]")
		_, port, _ = strings.Cut(rest, ":")
	} else {
		host, port, _ = strings.Cut(hostinfo, ":")
	}
	if name, zone, ok := strings.Cut(host, "%"); ok {
		host = strings.ToLower(name) + "%" + zone
	} else {
		host = strings.ToLower(host)
	}
	portText := ""
	if n, err := strconv.Atoi(port); err == nil && strings.Trim(port, "0123456789") == "" && n > 0 && n <= 65535 {
		portText = ":" + strconv.Itoa(n)
	}
	return scheme + "://" + host + portText, nil
}

const schemeChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+-."

func isASCIIAlpha(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

func validBracketedHost(host string) bool {
	if strings.HasPrefix(host, "v") {
		// IPvFuture, matched like CPython: v<hex>.<anything>
		hexPart, rest, ok := strings.Cut(host[1:], ".")
		return ok && hexPart != "" && strings.Trim(hexPart, "0123456789abcdefABCDEF") == "" && rest != ""
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.Is6()
}
