package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/LaneBirmingham/shartifacts/internal/auth"
	"github.com/LaneBirmingham/shartifacts/internal/store"
)

// Private app addresses. With PrivateAppOrigin set (https://*.apps.example.com), each app
// also answers at the root of a hostname of its own, https://budget.apps.example.com/, by
// the label its public links use (appLabel). The hostname is for the same people as the
// workspace: guard holds it to LAN, tailnet and loopback peers, refuses what a proxy forwards
// from the internet, and, with sign-in on, asks for a sign-in of its own. The public
// listener never serves it.
//
// A hostname of its own is what lets an app install on a phone as an app of its own. Under
// the workspace, an app's page sits inside the scope of the installed workspace, and Chrome
// on Android treats it as part of that app rather than offering another. At the root of its
// own origin it has a scope nothing else shares, and real localStorage, IndexedDB and
// cookies, apart from every other app's, as on its public hostname.
//
// The workspace's session cookie is host-only, so it never reaches these hostnames. A browser
// signs in to one by way of the workspace instead: GET /apps/{id}/open, signed in there, sends
// it to /_shartifacts/enter on the app's hostname with a one-time ticket good for a minute,
// which that hostname swaps for a cookie of its own. The cookie, like the ticket, is bound to
// the workspace session that asked (boundTo), so signing out there, or a new password, ends
// it too. Without one, a page there sends the browser back through the workspace.
//
// Everything at the root is the app's, except /_shartifacts/ (sign-in, manifest and icons),
// and /vendor/ and /media/, which serve the bundled libraries and the workspace's media
// when the app has no file of that name.

const (
	privateCookie       = "shartifacts_app"
	securePrivateCookie = "__Host-" + privateCookie
	// enterTTL is how long a ticket from /apps/{id}/open stays good: the length of a redirect.
	enterTTL = time.Minute
	// enterLabel and privateLabel mark what a signed session claim is for, so a ticket, a
	// cookie and a page's app session (labelled with its version) never stand in for each other.
	enterLabel   = "enter"
	privateLabel = "private"
)

type privateVisitKey struct{}

// privateVisit is what guard learned of a request to an app's private hostname.
type privateVisit struct {
	app    store.App
	signed bool // sign-in is off, or the request carries the hostname's cookie
}

func privateVisitOf(r *http.Request) (privateVisit, bool) {
	v, ok := r.Context().Value(privateVisitKey{}).(privateVisit)
	return v, ok
}

// enterTickets remembers the tickets already used, until they expire, so each works once.
type enterTickets struct {
	mu   sync.Mutex
	used map[string]time.Time
}

func (t *enterTickets) use(ticket string, expires time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if t.used == nil {
		t.used = map[string]time.Time{}
	}
	for k, e := range t.used {
		if now.After(e) {
			delete(t.used, k)
		}
	}
	if _, seen := t.used[ticket]; seen {
		return false
	}
	t.used[ticket] = expires
	return true
}

// privateLabelOf reports the label of a request to one of PrivateAppOrigin's hostnames.
func (s *Server) privateLabelOf(r *http.Request) (string, bool) {
	_, suffix, ok := strings.Cut(s.PrivateAppOrigin, "://*")
	if !ok {
		return "", false
	}
	label, ok := strings.CutSuffix(requestHost(r), strings.ToLower(suffix))
	if !ok || label == "" || strings.Contains(label, ".") {
		return "", false
	}
	return label, true
}

// privateApp is the app a private hostname's label names: its label, one it had before, or
// its generated one. current is false for the last two, whose pages move to the label it has.
func (s *Server) privateApp(label string) (a store.App, current, ok bool) {
	if id, found := s.Store.AppByLabel(label); found {
		if a, err := s.Store.App(id); err == nil {
			return a, s.appLabel(a) == label, true
		}
	}
	if generatedLabelPattern.MatchString(label) {
		for _, sum := range s.Store.Apps() {
			if s.generatedLabel(sum.ID) == label {
				if a, err := s.Store.App(sum.ID); err == nil {
					return a, s.appLabel(a) == label, true
				}
			}
		}
	}
	return store.App{}, false, false
}

// privateOrigin is an app's private origin, e.g. https://budget.apps.example.com.
func (s *Server) privateOrigin(a store.App) string {
	return strings.Replace(s.PrivateAppOrigin, "*", s.appLabel(a), 1)
}

// privateGuard runs in guard for a private hostname, after the network checks. It returns
// the request with what it learned, or false after answering it.
func (s *Server) privateGuard(w http.ResponseWriter, r *http.Request, label string) (*http.Request, bool) {
	a, current, ok := s.privateApp(label)
	if !ok {
		appNotFound(w, "No app has this address.")
		return r, false
	}
	read := r.Method == "GET" || r.Method == "HEAD"
	if !current {
		// The app has another label now. Send people there; nothing else answers here, so
		// no two hostnames hold the same app's storage.
		if read && r.Header.Get("Sec-Fetch-Mode") != "cors" && r.Header.Get("Sec-Fetch-Mode") != "no-cors" {
			http.Redirect(w, r, s.privateOrigin(a)+r.URL.RequestURI(), http.StatusFound)
		} else {
			appNotFound(w, "This app has moved to "+s.privateOrigin(a)+".")
		}
		return r, false
	}
	visit := privateVisit{app: a, signed: !s.signInOn() || s.privateCookieValid(r, a)}
	r = r.WithContext(context.WithValue(r.Context(), privateVisitKey{}, visit))
	if visit.signed || r.URL.Path == "/_shartifacts/enter" {
		return r, true
	}
	// Following a link, opening the installed app or reloading: sign in by way of the
	// workspace, which sends the browser back here. Anything else is refused.
	navigation := read && !strings.HasPrefix(r.URL.Path, "/api/") && (r.Header.Get("Sec-Fetch-Mode") == "navigate" || r.Header.Get("Sec-Fetch-Mode") == "")
	if navigation && s.PublicOrigin != "" {
		http.Redirect(w, r, strings.TrimRight(s.PublicOrigin, "/")+"/apps/"+a.ID+"/open?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		return r, false
	}
	if navigation {
		privatePage(w, 401, "Sign in through the workspace", "Open this app from its tab in the workspace with ⤓ Install, which signs this browser in here too.")
		return r, false
	}
	writeJSON(w, 401, map[string]string{"error": "sign in first: open this app from the workspace", "code": "unauthenticated"})
	return r, false
}

// privateCookieValid reports whether r carries this hostname's cookie for a, still bound to
// a workspace session. Every cookie of either name is tried, so one another site of the
// parent domain set cannot hide the real one.
func (s *Server) privateCookieValid(r *http.Request, a store.App) bool {
	for _, name := range []string{securePrivateCookie, privateCookie} {
		for _, c := range r.CookiesNamed(name) {
			if v, ok := s.verifySession(c.Value); ok && v.App == a.ID && v.Label == privateLabel && s.stillBound(v.Bind) {
				return true
			}
		}
	}
	return false
}

func (s *Server) privateAppRoutes(mux *http.ServeMux) {
	// The workspace's way into an app's private hostname, signed in. Its next is a path
	// there, which the hostname checks again (safeNext).
	mux.HandleFunc("GET /apps/{id}/open", func(w http.ResponseWriter, r *http.Request) {
		a, err := s.Store.App(r.PathValue("id"))
		if err != nil || s.PrivateAppOrigin == "" {
			appNotFound(w, "This app has no private address: set SHARTIFACTS_PRIVATE_APP_ORIGIN to give apps one.")
			return
		}
		next := safeNext(r.URL.Query().Get("next"))
		dest := s.privateOrigin(a)
		if !s.signInOn() {
			http.Redirect(w, r, dest+next, http.StatusSeeOther)
			return
		}
		nonce := make([]byte, 12)
		if _, err := rand.Read(nonce); err != nil {
			http.Error(w, "cannot sign in", 500)
			return
		}
		ticket := s.signSession(session{App: a.ID, Label: enterLabel, Bind: s.boundTo(r), Expires: time.Now().Add(enterTTL).Unix(), Nonce: base64.RawURLEncoding.EncodeToString(nonce)})
		http.Redirect(w, r, dest+"/_shartifacts/enter?"+url.Values{"t": {ticket}, "next": {next}}.Encode(), http.StatusSeeOther)
	})
}

// enterPrivateApp swaps a ticket from /apps/{id}/open for this hostname's cookie.
func (s *Server) enterPrivateApp(w http.ResponseWriter, r *http.Request, a store.App) {
	next := safeNext(r.URL.Query().Get("next"))
	if !s.signInOn() {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	t := r.URL.Query().Get("t")
	v, ok := s.verifySession(t)
	if !ok || v.App != a.ID || v.Label != enterLabel || !s.stillBound(v.Bind) || !s.enterTickets.use(t, time.Unix(v.Expires, 0)) {
		msg := "This sign-in link has expired or was already used."
		if s.PublicOrigin != "" {
			msg += ` <a href="` + html.EscapeString(strings.TrimRight(s.PublicOrigin, "/")+"/apps/"+a.ID+"/open?next="+url.QueryEscape(next)) + `">Try again</a>.`
		} else {
			msg += " Open the app again from the workspace."
		}
		privatePage(w, 403, "Sign in through the workspace", msg)
		return
	}
	value := s.signSession(session{App: a.ID, Label: privateLabel, Bind: v.Bind, Expires: time.Now().Add(auth.SessionTTL).Unix()})
	secure := s.clientScheme(r) == "https"
	name := privateCookie
	if secure {
		name = securePrivateCookie
	}
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: int(auth.SessionTTL.Seconds()), HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// servePrivateApp answers a request to an app's private hostname that guard let through.
func (s *Server) servePrivateApp(w http.ResponseWriter, r *http.Request, a store.App) {
	p := r.URL.Path
	read := r.Method == "GET" || r.Method == "HEAD"
	switch {
	case p == "/_shartifacts/enter" && read:
		s.enterPrivateApp(w, r, a)
		return
	case p == "/_shartifacts/manifest.webmanifest" && read:
		s.writeManifest(w, r, a, "/_shartifacts", "/", "/", "/")
		return
	case (p == "/_shartifacts/icon.png" || p == "/_shartifacts/icon-maskable.png") && read:
		s.serveLauncherIcon(w, r, a, p == "/_shartifacts/icon-maskable.png")
		return
	case strings.HasPrefix(p, "/_shartifacts/"):
		http.NotFound(w, r)
		return
	}
	if a.Live == 0 {
		appNotFound(w, "This app has no live version yet: its first deployment has not finished or did not build.")
		return
	}
	c := appCtx{app: a, version: a.Live, label: "live", base: "/", own: true, private: true}
	rel := strings.TrimPrefix(p, "/")
	// The libraries and media app pages load by absolute URL, unless the app has a file there.
	if read && (strings.HasPrefix(p, "/vendor/") || strings.HasPrefix(p, "/media/")) && !s.hasAppFile(c, rel) {
		if id, ok := strings.CutPrefix(p, "/media/"); ok {
			s.serveMedia(w, r, id)
			return
		}
		if tag := s.etag(p); tag != "" {
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("ETag", tag)
		}
		http.FileServerFS(s.Web).ServeHTTP(w, r)
		return
	}
	s.serveApp(w, r, c, rel)
}

// hasAppFile reports whether the app's frontend has a file at rel.
func (s *Server) hasAppFile(c appCtx, rel string) bool {
	site := s.site(c.app.ID, c.version)
	if !site.Frontend {
		return false
	}
	f, ok := s.frontendFile(c.app.ID, c.version, site, rel)
	return ok && f != "dir"
}

// withoutCookies removes the named cookies from a request's Cookie headers, keeping the
// others as they were written.
func withoutCookies(h http.Header, names ...string) {
	var kept []string
	for _, line := range h.Values("Cookie") {
		for _, part := range strings.Split(line, ";") {
			name, _, _ := strings.Cut(strings.TrimSpace(part), "=")
			drop := false
			for _, n := range names {
				drop = drop || name == n
			}
			if !drop && strings.TrimSpace(part) != "" {
				kept = append(kept, strings.TrimSpace(part))
			}
		}
	}
	h.Del("Cookie")
	if len(kept) > 0 {
		h.Set("Cookie", strings.Join(kept, "; "))
	}
}

func privatePage(w http.ResponseWriter, status int, title, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>%[1]s</title><style>body{font:16px/1.5 system-ui,sans-serif;color:#403b38;background:#f9f8f6;display:grid;place-items:center;min-height:90vh;margin:0}main{max-width:420px;padding:24px;text-align:center}a{color:inherit}</style><main><h1 style="font:400 28px Georgia,serif">%[1]s</h1><p>%[2]s</p></main>`, html.EscapeString(title), msg)
}
