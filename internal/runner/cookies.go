package runner

import (
	"context"
	"net/http"
	"strings"
)

// Cookies is what a backend's Set-Cookie headers may do on their way to the browser. Which
// one applies depends on the origin the page was served from, so shartifacts sets it on
// each proxied request (WithCookies); the proxy applies it to the response, upgrades
// included.
type Cookies int

const (
	// PassCookies leaves them alone (a request with no policy, as in the sidecar, whose
	// caller applies its own).
	PassCookies Cookies = iota
	// DropCookies removes them: the page shares its origin with the workspace or with
	// other apps, so no cookie there is the app's to set.
	DropCookies
	// HostOnlyCookies keeps them without Domain attributes, so they stay on the app's own
	// hostname instead of reaching the parent domain's other sites.
	HostOnlyCookies
)

type cookiesKey struct{}

// WithCookies sets the policy for the responses to requests made with ctx.
func WithCookies(ctx context.Context, c Cookies) context.Context {
	return context.WithValue(ctx, cookiesKey{}, c)
}

func filterCookies(res *http.Response) {
	c, _ := res.Request.Context().Value(cookiesKey{}).(Cookies)
	switch c {
	case DropCookies:
		res.Header.Del("Set-Cookie")
	case HostOnlyCookies:
		set := res.Header.Values("Set-Cookie")
		res.Header.Del("Set-Cookie")
		for _, v := range set {
			res.Header.Add("Set-Cookie", hostOnly(v))
		}
	}
}

// hostOnly drops a Set-Cookie value's Domain attributes.
func hostOnly(cookie string) string {
	parts := strings.Split(cookie, ";")
	out := parts[:1:1]
	for _, p := range parts[1:] {
		name, _, _ := strings.Cut(p, "=")
		if !strings.EqualFold(strings.TrimSpace(name), "domain") {
			out = append(out, p)
		}
	}
	return strings.Join(out, ";")
}
