package server

import (
	"log"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"time"
)

// The workspace is for private networks: who can reach it is its first access control
// (sign-in, when it is on, comes after), and the peer allowlist in guard enforces that for
// direct connections. A reverse proxy hides the
// peer, because every request then arrives from the proxy's own private address. If that
// proxy is also reachable from the internet for the workspace's hostname (a Caddy or nginx
// on the same VPS, a Cloudflare tunnel with the wrong hostname), the allowlist passes
// strangers.
//
// Forwarded headers cannot be trusted to grant access, since anything that reaches the port
// directly can write them. They can be trusted to deny it: a header naming a public client
// only ever costs the sender its own request. So when one names a public address, the
// workspace refuses, unless the operator says the proxy authenticates people (AuthProxy).
// Share and app hostnames are public by design and are not affected.

var forwardedHeaders = []string{"X-Forwarded-For", "X-Real-Ip", "Forwarded", "Cf-Connecting-Ip", "True-Client-Ip", "X-Client-Ip", "Fly-Client-Ip"}

// forwardedPublic returns the first public client address a proxy reported, if any.
func forwardedPublic(r *http.Request) (string, bool) {
	for _, name := range forwardedHeaders {
		for _, header := range r.Header.Values(name) {
			for _, part := range strings.Split(header, ",") {
				if name == "Forwarded" {
					part = forwardedFor(part)
				}
				if ip, ok := parseForwardedAddr(part); ok && !localIP(ip) {
					return ip.String(), true
				}
			}
		}
	}
	return "", false
}

// forwardedFor extracts the for= value of one RFC 7239 Forwarded element.
func forwardedFor(element string) string {
	for _, pair := range strings.Split(element, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if ok && strings.EqualFold(k, "for") {
			return v
		}
	}
	return ""
}

// parseForwardedAddr reads an address as proxies write it: bare, with a port, bracketed,
// or quoted. Names and obfuscated identifiers ("unknown", "_hidden") are not addresses.
func parseForwardedAddr(v string) (netip.Addr, bool) {
	v = strings.Trim(strings.TrimSpace(v), `"`)
	if ip, err := netip.ParseAddr(v); err == nil {
		return ip.Unmap(), true
	}
	if ap, err := netip.ParseAddrPort(v); err == nil {
		return ap.Addr().Unmap(), true
	}
	if ip, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(v, "["), "]")); err == nil {
		return ip.Unmap(), true
	}
	return netip.Addr{}, false
}

var lastForwardedLog atomic.Int64

// refuseForwardedPublic answers a workspace request a proxy forwarded from a public client,
// and logs it at most once a minute so a misconfigured proxy is noticed without flooding.
func refuseForwardedPublic(w http.ResponseWriter, r *http.Request, client string) {
	now := time.Now().Unix()
	if last := lastForwardedLog.Load(); now-last >= 60 && lastForwardedLog.CompareAndSwap(last, now) {
		log.Printf("refused %s %s for %s: a proxy forwarded it from public address %s. The workspace must stay on your LAN or tailnet: publish only the share hostname, or start with -auth-proxy if the proxy authenticates people", r.Method, r.URL.Path, r.Host, client)
	}
	http.Error(w, "local network access only: this request reached the workspace through a proxy from a public address. the workspace stays on your LAN or tailnet, with or without sign-in; only share links are public.", 403)
}
