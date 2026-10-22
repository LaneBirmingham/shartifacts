package server

import (
	"net/http"
	"net/url"
	"strings"
)

// TLSAllowedPath answers a reverse proxy that gets certificates on demand (Caddy's
// on_demand_tls "ask"): 200 for a hostname this server serves publicly, 404 for any other.
// With it, per-app hostnames need no wildcard certificate and so no DNS provider API: the
// proxy gets each app's certificate the first time someone opens one of its links, and
// refuses names nobody gave an app, which keeps strangers from spending its rate limits.
const TLSAllowedPath = "/api/tls-allowed"

// tlsAllowed serves TLSAllowedPath. The caller is the proxy itself, on the same machine or
// compose network: guard has already required a private peer, and a request a proxy
// forwarded, or one for a public hostname, is not the proxy asking.
func (s *Server) tlsAllowed(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "HEAD" {
		http.Error(w, "method not allowed", 405)
		return
	}
	_, appHost := s.appHost(r)
	if appHost || s.isShareHost(r) || forwarded(r) {
		http.NotFound(w, r)
		return
	}
	domain := strings.TrimSuffix(strings.ToLower(r.URL.Query().Get("domain")), ".")
	if domain == "" || !s.servesPublicly(domain) {
		http.Error(w, "not a hostname this server serves", 404)
		return
	}
	w.WriteHeader(200)
}

// servesPublicly reports whether a hostname is the share hostname or an app's, by a label
// an app has now or had before (its links redirect from those).
func (s *Server) servesPublicly(host string) bool {
	if u, err := url.Parse(s.ShareOrigin); err == nil && s.ShareOrigin != "" && strings.EqualFold(u.Hostname(), host) {
		return true
	}
	_, suffix, ok := strings.Cut(s.AppOrigin, "://*")
	if !ok {
		return false
	}
	if i := strings.LastIndex(suffix, ":"); i > 0 {
		suffix = suffix[:i]
	}
	label, ok := strings.CutSuffix(host, strings.ToLower(suffix))
	return ok && label != "" && !strings.Contains(label, ".") && s.appLabelInUse(label)
}

func forwarded(r *http.Request) bool {
	for _, name := range forwardedHeaders {
		if r.Header.Get(name) != "" {
			return true
		}
	}
	return false
}
