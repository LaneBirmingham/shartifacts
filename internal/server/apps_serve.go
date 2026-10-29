package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/LaneBirmingham/shartifacts/internal/runner"
	"github.com/LaneBirmingham/shartifacts/internal/store"
)

// Serving apps. An app version lives under a base URL:
//
//	/run/{id}/live/   whichever version is live (what tabs and links show)
//	/run/{id}/{n}/    one version, for previews and rollback checks
//	/p/{token}/       a public share link, on the share origin
//
// Beneath it, api/... goes to the version's backend (with the base stripped, so the backend
// sees /api/...), and everything else is the frontend: built pages and bundles first, then
// the deployed files, then index.html for client-side routes. Pages run in the same
// opaque-origin sandbox as artifacts, so they cannot reach the workspace API; their calls to
// their own backend are cross-origin requests from "null", which this server lets through
// only with the session token it put in the page (or, on a share link, the link's token).
// A small runtime injected into each page adds the token to fetch, XHR, EventSource and
// WebSocket requests under the app's base, so apps call relative URLs (fetch("api/todos"))
// with no setup.
//
// With AppOrigin set, public links move to a hostname of the app's own,
// https://{label}.share.example.com/p/{token}/, one label per app: one it was given, like
// budget, or else one generated from its id. There the page keeps
// its origin, so the browser gives it real localStorage, IndexedDB and cookies, which no
// other app can read; the hostname answers only that app's links and only its own pages'
// requests, and its backend's cookies stay on it. Elsewhere pages stay in the opaque
// sandbox, where a hub tab keeps localStorage for them (appRuntime).

// SessionHeader carries a page's session token on its backend calls; sessionParam carries
// it where headers cannot go (EventSource, WebSocket).
const (
	SessionHeader = "X-Shartifacts-Session"
	sessionParam  = "__st"
	sessionTTL    = 30 * 24 * time.Hour
)

// appCtx is one request's app version and the URL it was reached through.
type appCtx struct {
	app     store.App
	version int
	label   string // "live", the version number, or "share"
	base    string // URL path of the version's root, with a trailing slash
	public  bool   // reached through a share link
	own     bool   // on the app's own origin (AppOrigin), not a sandbox
	share   store.AppShare
}

func (c appCtx) deployment() store.Deployment { return c.app.Deployments[c.version-1] }

// session is what a page's token vouches for. Bind ties it to the sign-in it was served
// under (boundTo), so signing out, a new password or revoking a token ends it too.
type session struct {
	App     string `json:"a"`
	Label   string `json:"l"`
	Viewer  string `json:"v,omitempty"`
	Bind    string `json:"b,omitempty"`
	Expires int64  `json:"e"`
}

func (s *Server) appKey() []byte {
	s.appKeyOnce.Do(func() {
		k, err := s.Store.Secret("app-session")
		if err != nil {
			panic(fmt.Sprintf("app session key: %v", err))
		}
		s.appSessionKey = k
	})
	return s.appSessionKey
}

func (s *Server) signSession(v session) string {
	b, _ := json.Marshal(v)
	payload := base64.RawURLEncoding.EncodeToString(b)
	mac := hmac.New(sha256.New, s.appKey())
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) verifySession(token string) (session, bool) {
	payload, sig, ok := strings.Cut(token, ".")
	if !ok {
		return session{}, false
	}
	mac := hmac.New(sha256.New, s.appKey())
	mac.Write([]byte(payload))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(want)) {
		return session{}, false
	}
	b, err := base64.RawURLEncoding.DecodeString(payload)
	var v session
	if err != nil || json.Unmarshal(b, &v) != nil || time.Now().Unix() > v.Expires {
		return session{}, false
	}
	return v, true
}

// appPath reports whether a request is for an app's pages or backend; those make their own
// cross-origin decisions (serveApp), since sandboxed pages always call cross-origin.
func appPath(p string) bool { return strings.HasPrefix(p, "/run/") || strings.HasPrefix(p, "/p/") }

func (s *Server) appServeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/run/{id}/{ver}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path+"/"+query(r), http.StatusFound)
	})
	mux.HandleFunc("/run/{id}/{ver}/{path...}", func(w http.ResponseWriter, r *http.Request) {
		a, err := s.Store.App(r.PathValue("id"))
		if err != nil {
			appNotFound(w, "No such app.")
			return
		}
		label := r.PathValue("ver")
		v := a.Live
		if label != "live" {
			if v, err = strconv.Atoi(label); err != nil || v < 1 || v > len(a.Deployments) || strconv.Itoa(v) != label {
				appNotFound(w, "No such version.")
				return
			}
		}
		if v == 0 {
			appNotFound(w, "This app has no live version yet: its first deployment has not finished or did not build.")
			return
		}
		s.serveApp(w, r, appCtx{app: a, version: v, label: label, base: "/run/" + a.ID + "/" + label + "/"}, r.PathValue("path"))
	})
	mux.HandleFunc("/p/{token}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path+"/"+query(r), http.StatusFound)
	})
	mux.HandleFunc("/p/{token}/{path...}", func(w http.ResponseWriter, r *http.Request) {
		sh, a, ok := s.Store.AppShared(r.PathValue("token"))
		label, own := s.appHost(r)
		moved := false // on a hostname the app had before
		if own && ok && s.appLabel(a) != label {
			moved = label == s.generatedLabel(a.ID) || slices.Contains(a.FormerLabels, label)
			ok = moved // otherwise another app's link: each hostname answers its own app's only
		}
		if !ok {
			if own && !s.appLabelInUse(label) {
				// The app is gone: let browsers drop what its pages kept. Never "cookies",
				// which would clear every cookie of the parent domain.
				w.Header().Set("Clear-Site-Data", `"storage"`)
			}
			appNotFound(w, "This link was revoked or never existed.")
			return
		}
		p := r.PathValue("path")
		if (moved || s.AppOrigin != "" && s.isShareHost(r)) && (r.Method == "GET" || r.Method == "HEAD") && p != "api" && !strings.HasPrefix(p, "api/") {
			// Pages moved to the app's own origin; the backend still answers here for
			// clients that call it directly, and for pages already open here.
			http.Redirect(w, r, s.appShareURL(a, sh.Token)+p+query(r), http.StatusFound)
			return
		}
		v := sh.Version
		if v == 0 {
			v = a.Live
		}
		if v == 0 {
			appNotFound(w, "This app has no live version right now.")
			return
		}
		s.serveApp(w, r, appCtx{app: a, version: v, label: "share", base: "/p/" + sh.Token + "/", public: true, own: own, share: sh}, p)
	})
}

func query(r *http.Request) string {
	if r.URL.RawQuery == "" {
		return ""
	}
	return "?" + r.URL.RawQuery
}

func appNotFound(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	w.WriteHeader(404)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Not found</title><style>body{font:16px/1.5 system-ui,sans-serif;color:#403b38;background:#f9f8f6;display:grid;place-items:center;min-height:90vh;margin:0}main{max-width:420px;padding:24px;text-align:center}</style><main><h1 style="font:400 28px Georgia,serif">Nothing here</h1><p>%s</p></main>`, html.EscapeString(msg))
}

func (s *Server) serveApp(w http.ResponseWriter, r *http.Request, c appCtx, p string) {
	// The mux cleans dot segments only as written; encoded ones (%2e%2e) arrive decoded
	// here, and must not reach a backend or a file lookup as a real "..".
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
	}
	d := c.deployment()
	if d.Status != store.StatusReady || d.Pruned {
		appNotFound(w, fmt.Sprintf("Version %d is %s.", d.Version, map[bool]string{true: "pruned", false: d.Status}[d.Pruned]))
		return
	}
	if p == "api" || strings.HasPrefix(p, "api/") {
		s.serveAppAPI(w, r, c, p)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "the frontend is static; send writes to api/", http.StatusMethodNotAllowed)
		return
	}
	s.serveAppFile(w, r, c, p)
}

// serveAppAPI proxies a request to the version's backend.
func (s *Server) serveAppAPI(w http.ResponseWriter, r *http.Request, c appCtx, p string) {
	origin := r.Header.Get("Origin")
	crossOrigin := origin != "" && origin != s.clientScheme(r)+"://"+r.Host
	if c.own {
		// The app's own origin answers its own pages only (guard), so it needs no CORS.
		if crossOrigin {
			writeJSON(w, 403, map[string]string{"error": "cross-origin request denied"})
			return
		}
	} else {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Expose-Headers", "*")
	}
	if r.Method == "OPTIONS" && crossOrigin && r.Header.Get("Access-Control-Request-Method") != "" {
		w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS")
		if h := r.Header.Get("Access-Control-Request-Headers"); h != "" {
			w.Header().Set("Access-Control-Allow-Headers", h)
		}
		w.Header().Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if c.deployment().Backend == "" {
		writeJSON(w, 404, map[string]string{"error": "this app has no backend"})
		return
	}
	viewer := s.viewerID(r)
	// Only the app's own sandboxed page ("null") may call cross-origin, and on the workspace
	// host only with the session token served in that page. With sign-in on, so must any
	// request that is not signed in.
	if crossOrigin && origin != "null" {
		writeJSON(w, 403, map[string]string{"error": "cross-origin request denied"})
		return
	}
	if !c.public && (crossOrigin || !s.signedIn(r)) {
		token := r.Header.Get(SessionHeader)
		if token == "" {
			token = r.URL.Query().Get(sessionParam)
		}
		ses, ok := s.verifySession(token)
		if !ok || ses.App != c.app.ID || ses.Label != c.label || !s.stillBound(ses.Bind) {
			if crossOrigin {
				writeJSON(w, 403, map[string]string{"error": "missing or expired app session: reload the page"})
			} else {
				writeJSON(w, 401, map[string]string{"error": "sign in first", "code": "unauthenticated"})
			}
			return
		}
		viewer = ses.Viewer
	}
	upgrade := r.Header.Get("Upgrade") != ""
	if c.public {
		viewer = ""
		// CORS preflights were answered above. A WebSocket carries writes after its GET.
		if c.share.ReadOnly && (r.Method != "GET" && r.Method != "HEAD" || upgrade) {
			writeJSON(w, 403, map[string]string{"error": "this link is read-only"})
			return
		}
	}
	// Cookies are the app's only on its own origin. Elsewhere the page shares its origin
	// (the workspace, or every app on the share hostname), so none pass either way.
	cookies := runner.DropCookies
	if c.own {
		cookies = runner.HostOnlyCookies
	}
	out := r.Clone(runner.WithCookies(r.Context(), cookies))
	if !c.own {
		out.Header.Del("Cookie")
	}
	out.URL.Path = "/" + p
	out.URL.RawPath = ""
	q := out.URL.Query()
	if q.Has(sessionParam) || q.Has("viewer") {
		q.Del(sessionParam)
		q.Del("viewer")
		out.URL.RawQuery = q.Encode()
	}
	for name := range out.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-shartifacts-") || strings.HasPrefix(strings.ToLower(name), "x-forwarded-") {
			out.Header.Del(name)
		}
	}
	// An agent's workspace token is for shartifacts, not the app: a backend that saw it could
	// use the whole workspace. An app's own Authorization header passes.
	if workspaceToken(out.Header) {
		out.Header.Del("Authorization")
	}
	// The proxy drops every header Connection names, which would let a client delete the
	// trusted headers set below. Keep only what an upgrade needs.
	out.Header.Del("Connection")
	if upgrade {
		out.Header.Set("Connection", "Upgrade")
	}
	out.Header.Set("X-Forwarded-Prefix", strings.TrimSuffix(c.base, "/"))
	out.Header.Set("X-Forwarded-Host", r.Host)
	out.Header.Set("X-Forwarded-Proto", s.clientScheme(r))
	out.Header.Set("X-Shartifacts-Version", strconv.Itoa(c.version))
	access := "lan"
	if c.public {
		access = "public"
		if c.share.ReadOnly {
			access = "public-read-only"
		}
	}
	out.Header.Set("X-Shartifacts-Access", access)
	if person, ok := s.Store.Person(viewer); ok && viewer != "" {
		b, _ := json.Marshal(map[string]string{"id": person.ID, "name": person.Name})
		out.Header.Set("X-Shartifacts-User", string(b))
	}
	// WebSockets and event streams outlive the server's five-minute write timeout.
	if upgrade || strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		rc := http.NewResponseController(w)
		rc.SetReadDeadline(time.Time{})
		rc.SetWriteDeadline(time.Time{})
		if c.public {
			// Revoking the link, or deleting the app, ends the connection too.
			ctx, cancel := context.WithCancel(out.Context())
			defer cancel()
			go s.whileShared(ctx, cancel, c.share.Token)
			out = out.WithContext(ctx)
		}
	}
	s.Runtime.Proxy(w, out, c.app.ID, c.version)
}

// whileShared cancels a public link's long-lived connection once the link stops existing.
func (s *Server) whileShared(ctx context.Context, cancel context.CancelFunc, token string) {
	tick := time.NewTicker(linkCheckInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if _, _, ok := s.Store.AppShared(token); !ok {
				cancel()
				return
			}
		}
	}
}

// linkCheckInterval is how soon a revoked link's open streams end.
var linkCheckInterval = 5 * time.Second

// site reads a version's out/site.json, once.
func (s *Server) site(id string, version int) appSite {
	k := id + "/" + strconv.Itoa(version)
	if v, ok := s.appSites.Load(k); ok {
		return v.(appSite)
	}
	var site appSite
	if b, err := os.ReadFile(filepath.Join(s.Store.AppBuildDir(id, version), "site.json")); err == nil {
		json.Unmarshal(b, &site)
	}
	s.appSites.Store(k, site)
	return site
}

func (s *Server) serveAppFile(w http.ResponseWriter, r *http.Request, c appCtx, p string) {
	site := s.site(c.app.ID, c.version)
	clean := strings.TrimPrefix(path.Clean("/"+p), "/")
	if strings.HasSuffix(p, "/") && clean != "" {
		clean += "/"
	}
	if !site.Frontend {
		if clean == "" {
			s.writeAppPage(w, r, c, "", []byte(`<!doctype html><meta charset="utf-8"><title>API</title><body style="font:16px/1.5 system-ui,sans-serif;padding:32px;color:#403b38;background:#f9f8f6"><h1 style="font:400 28px Georgia,serif">This app is an API.</h1><p>It has no pages. Its backend answers under <code>api/</code>.</p>`))
			return
		}
		http.NotFound(w, r)
		return
	}
	find := func(rel string) (string, bool) { return s.frontendFile(c.app.ID, c.version, site, rel) }
	rel := clean
	f, ok := find(rel)
	if ok && f == "dir" {
		http.Redirect(w, r, c.base+rel+"/"+query(r), http.StatusFound)
		return
	}
	if !ok && site.SPA && path.Ext(strings.TrimSuffix(rel, "/")) == "" {
		rel = ""
		f, ok = find("")
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(rel, "/") || rel == "" {
		rel += "index.html"
	}
	switch path.Ext(f) {
	case ".html", ".htm":
		b, err := os.ReadFile(f)
		if err != nil {
			http.Error(w, "unreadable page", 500)
			return
		}
		s.writeAppPage(w, r, c, path.Dir(rel), b)
		return
	}
	t := mime.TypeByExtension(path.Ext(f))
	if t == "" {
		t = "application/octet-stream"
	}
	w.Header().Set("Content-Type", t)
	// Module scripts, fonts and fetches from the opaque-origin page are CORS requests.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	if c.label != "live" && c.label != "share" {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeFile(w, r, f)
}

// frontendFile returns the file for a path under a version's web root: built output, the
// deployed file, or public/ (Vite's folder of files served at the root). A directory
// without a trailing slash is "dir", for the caller to redirect.
func (s *Server) frontendFile(id string, version int, site appSite, rel string) (string, bool) {
	if rel != "" && (serverOnly(path.Join(site.Root, rel), site) || store.ValidAppPath(strings.TrimSuffix(rel, "/")) != nil) {
		return "", false
	}
	out := filepath.Join(s.Store.AppBuildDir(id, version), "public")
	root := filepath.Join(s.Store.AppSourceDir(id, version), filepath.FromSlash(site.Root))
	for _, dir := range []string{out, root, filepath.Join(root, "public")} {
		f := filepath.Join(dir, filepath.FromSlash(rel))
		info, err := os.Stat(f)
		if err == nil && info.IsDir() {
			if !strings.HasSuffix(rel, "/") && rel != "" {
				return "dir", true
			}
			f = filepath.Join(f, "index.html")
			info, err = os.Stat(f)
		}
		if err == nil && !info.IsDir() {
			return f, true
		}
	}
	return "", false
}

var (
	headOpen = regexp.MustCompile(`(?i)<head\b[^>]*>`)
	htmlOpen = regexp.MustCompile(`(?i)<html\b[^>]*>`)
	baseTag  = regexp.MustCompile(`(?i)<base\b`)
)

// writeAppPage serves an app page: the runtime and a <base> for the page's directory
// injected first thing in <head>, under the sandboxing CSP.
func (s *Server) writeAppPage(w http.ResponseWriter, r *http.Request, c appCtx, dir string, page []byte) {
	origin := s.clientScheme(r) + "://" + r.Host
	ws := "ws" + strings.TrimPrefix(origin, "http")
	root := origin + c.base
	pageBase := c.base
	if dir != "." && dir != "" {
		pageBase += dir + "/"
	}
	viewer := ""
	if !c.public {
		viewer = s.viewerID(r)
	}
	cfg := map[string]any{"app": c.app.ID, "name": c.app.Name, "version": c.version, "live": c.label == "live" || (c.public && c.share.Version == 0),
		"base": c.base, "api": c.base + "api/", "public": c.public, "readOnly": c.public && c.share.ReadOnly, "user": nil}
	if person, ok := s.Store.Person(viewer); ok && viewer != "" {
		cfg["user"] = map[string]string{"id": person.ID, "name": person.Name}
	}
	// With sign-in on, a page loaded without it (by its own opaque-origin frame, or by
	// someone who only knows the app's id) gets no session, so its backend refuses it.
	session := ""
	if !c.public && s.signedIn(r) {
		session = s.signSession(sessionClaims(c, viewer, s.boundTo(r)))
	}
	cfgJSON, _ := json.Marshal(cfg)
	sessJSON, _ := json.Marshal(session)
	inject := `<base href="` + html.EscapeString(pageBase) + `">`
	if baseTag.Match(page) {
		inject = ""
	}
	inject += "<script>" + strings.NewReplacer("__CONFIG__", string(cfgJSON), "__SESSION__", string(sessJSON), "</", `<\/`).Replace(appRuntime) + "</script>"
	text := string(page)
	if loc := headOpen.FindStringIndex(text); loc != nil {
		text = text[:loc[1]] + inject + text[loc[1]:]
	} else if loc := htmlOpen.FindStringIndex(text); loc != nil {
		text = text[:loc[1]] + "<head>" + inject + "</head>" + text[loc[1]:]
	} else {
		text = inject + text
	}
	media := ""
	if !c.public {
		media = " " + origin + "/media/"
	}
	// On its own origin the page keeps that origin, and with it its storage and cookies;
	// the sandbox's other limits stay.
	sandbox := previewSandbox
	if c.own {
		sandbox += " allow-same-origin"
	}
	csp := fmt.Sprintf("default-src 'none'; script-src 'unsafe-inline' 'unsafe-eval' %[1]s/vendor/ %[2]s; style-src 'unsafe-inline' %[1]s/vendor/ %[2]s; img-src data: blob: %[2]s%[4]s; media-src data: blob: %[2]s%[4]s; font-src data: %[1]s/vendor/fonts/ %[2]s; connect-src %[2]s %[3]s%[5]s; worker-src blob: %[2]s; manifest-src %[2]s; frame-src 'none'; object-src 'none'; base-uri %[2]s; form-action %[2]s; frame-ancestors 'self'; sandbox %[6]s",
		origin, root, ws, media, c.base, sandbox)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprint(w, text)
}

func sessionClaims(c appCtx, viewer, bind string) session {
	return session{App: c.app.ID, Label: c.label, Viewer: viewer, Bind: bind, Expires: time.Now().Add(sessionTTL).Unix()}
}

// appRuntime runs first in every app page. It exposes window.shartifacts (the app, version,
// base URL and viewer), adds the session token to requests under the app's base (form
// submissions included), gives
// in-memory localStorage/sessionStorage when the sandbox refuses the real ones, opens
// outside links in a new tab, and reports errors to the hub showing the page.
//
// A hub tab keeps that storage: the hub names the page's frame with the app's saved
// localStorage (and the tab's sessionStorage), which the page reads synchronously before
// its own scripts run, and the page posts back what it writes. It also writes it into its
// frame's name, so a reload inside the frame keeps it. A page of another app sees no
// storage it did not write.
const appRuntime = `(()=>{const C=__CONFIG__,T=__SESSION__,H='X-Shartifacts-Session';
window.shartifacts=Object.freeze(C);
const root=new URL(C.base,location.href).href;
const ours=u=>{try{const x=new URL(String(u),document.baseURI);if(x.protocol==='ws:')x.protocol='http:';if(x.protocol==='wss:')x.protocol='https:';return x.href.startsWith(root)}catch(e){return false}};
const tag=u=>{if(!T||!ours(u))return u;const x=new URL(String(u),document.baseURI);x.searchParams.set('__st',T);return x.href};
const f=window.fetch;
window.fetch=function(input,init){try{const u=input instanceof Request?input.url:input;if(T&&ours(u)){const h=new Headers(init&&init.headers||(input instanceof Request?input.headers:undefined));h.set(H,T);init=Object.assign({},init,{headers:h})}}catch(e){}return f.call(this,input,init)};
const X=XMLHttpRequest.prototype,xo=X.open,xs=X.send;
X.open=function(m,u){this.__shs=!!T&&ours(u);return xo.apply(this,arguments)};
X.send=function(){if(this.__shs)this.setRequestHeader(H,T);return xs.apply(this,arguments)};
if(window.EventSource){const E=window.EventSource,P=function(u,o){return new E(tag(u),o)};P.prototype=E.prototype;P.CONNECTING=0;P.OPEN=1;P.CLOSED=2;window.EventSource=P}
if(window.WebSocket){const W=window.WebSocket,P=function(u,p){let x=new URL(String(u),document.baseURI);if(x.protocol==='http:')x.protocol='ws:';if(x.protocol==='https:')x.protocol='wss:';return new W(tag(x.href),p)};P.prototype=W.prototype;P.CONNECTING=0;P.OPEN=1;P.CLOSING=2;P.CLOSED=3;window.WebSocket=P}
const SP='shartifacts-storage:',kept=(()=>{let x=null;if(!String(window.name).startsWith(SP))return null;try{x=JSON.parse(window.name.slice(SP.length))}catch(e){}if(x&&x.app===C.app&&x.local&&typeof x.local==='object')return x;window.name='';return null})();
const mem={};let due=0;
const save=()=>{due=0;const local=Object.fromEntries(mem.localStorage||[]),session=Object.fromEntries(mem.sessionStorage||[]);try{window.name=SP+JSON.stringify({app:C.app,local,session})}catch(e){}if(parent!==window)parent.postMessage({type:'shartifacts-app-storage',app:C.app,local,session},'*')};
const changed=()=>{if(kept&&!due){due=1;setTimeout(save,0)}};
addEventListener('pagehide',()=>{if(due)save()});
for(const k of['localStorage','sessionStorage']){try{window[k].length}catch(e){const init=kept&&(k==='localStorage'?kept.local:kept.session),m=new Map(Object.entries(init&&typeof init==='object'?init:{}).map(([a,b])=>[a,String(b)]));mem[k]=m;let size=0;for(const[a,b]of m)size+=a.length+b.length;const s={get length(){return m.size},key:i=>[...m.keys()][i]??null,getItem:k=>m.has(String(k))?m.get(String(k)):null,setItem:(k,v)=>{k=String(k);v=String(v);const d=k.length+v.length-(m.has(k)?k.length+m.get(k).length:0);if(size+d>5e6)throw new DOMException('This app\'s storage is full.','QuotaExceededError');size+=d;m.set(k,v);changed()},removeItem:k=>{k=String(k);if(m.has(k)){size-=k.length+m.get(k).length;m.delete(k);changed()}},clear:()=>{if(m.size){m.clear();size=0;changed()}}};try{Object.defineProperty(window,k,{value:s,configurable:true})}catch(e){}}}
const seen=new Set(),report=m=>{m=String(m).slice(0,2000);if(parent!==window&&seen.size<20&&!seen.has(m)){seen.add(m);parent.postMessage({type:'shartifacts-app-error',app:C.app,version:C.version,message:m},'*')}};
addEventListener('error',e=>{if(e.target===window)report(e.message+(e.filename?' ('+e.filename.replace(root,'')+':'+e.lineno+')':''));else if(e.target&&e.target.tagName)report('failed to load <'+e.target.tagName.toLowerCase()+'> '+(e.target.currentSrc||e.target.src||e.target.href||''))},true);
addEventListener('unhandledrejection',e=>report('unhandled rejection: '+String(e.reason&&e.reason.stack||e.reason)));
addEventListener('securitypolicyviolation',e=>report('blocked by the sandbox: '+e.blockedURI+' ('+e.effectiveDirective+')'));
document.addEventListener('submit',e=>{const f=e.target;if(f&&f.action&&ours(f.action))f.action=tag(f.action)},true);
document.addEventListener('click',e=>{const a=e.target.closest&&e.target.closest('a[href]');if(!a||e.defaultPrevented)return;const h=a.getAttribute('href');if(!h||h[0]==='#'||/^javascript:/i.test(h)||a.hasAttribute('download'))return;if(ours(a.href))return;if(!a.target||/^_(self|parent|top)$/i.test(a.target)){a.target='_blank';a.rel='noopener noreferrer'}});
addEventListener('load',()=>{if(parent!==window)parent.postMessage({type:'shartifacts-app-ready',app:C.app,version:C.version,title:document.title},'*')});
})();`

// appURL is a version's base URL as served to a browser.
func appURL(base, id string, version int, live bool) string {
	if live {
		return base + "/run/" + id + "/live/"
	}
	return base + "/run/" + id + "/" + strconv.Itoa(version) + "/"
}

func (s *Server) appShareURL(a store.App, token string) string {
	if s.ShareOrigin == "" {
		return ""
	}
	if s.AppOrigin != "" {
		return s.appLinkOrigin(a) + "/p/" + token + "/"
	}
	return strings.TrimRight(s.ShareOrigin, "/") + "/p/" + token + "/"
}

// appLinkOrigin is the origin of an app's public links on AppOrigin.
func (s *Server) appLinkOrigin(a store.App) string {
	return strings.Replace(s.AppOrigin, "*", s.appLabel(a), 1)
}

// appLabel is an app's hostname label on AppOrigin: the same for every link to the app, so
// what its pages keep in a browser outlasts any one link. It is the label the app was given,
// or else its generated one.
func (s *Server) appLabel(a store.App) string {
	if a.Label != "" {
		return a.Label
	}
	return s.generatedLabel(a.ID)
}

// generatedLabel is the label of an app given none: not the app's id, which stays private,
// but 80 bits of an HMAC of it, in 16 lowercase base32 characters. It stays the app's after
// it is given a label, and its links redirect there.
func (s *Server) generatedLabel(id string) string {
	s.appLabelOnce.Do(func() {
		k, err := s.Store.Secret("app-origin")
		if err != nil {
			panic(fmt.Sprintf("app origin key: %v", err))
		}
		s.appLabelKey = k
	})
	mac := hmac.New(sha256.New, s.appLabelKey)
	mac.Write([]byte(id))
	return strings.ToLower(base32.StdEncoding.EncodeToString(mac.Sum(nil)[:10]))
}

var generatedLabelPattern = regexp.MustCompile(`^[a-z2-7]{16}$`)

// appLabelInUse reports whether a label is an existing app's.
func (s *Server) appLabelInUse(label string) bool {
	if _, ok := s.Store.AppByLabel(label); ok {
		return true
	}
	for _, a := range s.Store.Apps() {
		if s.generatedLabel(a.ID) == label {
			return true
		}
	}
	return false
}

// appHost reports the app label of a request to one of AppOrigin's hostnames: a generated
// label's shape, or a label an app was given. No other name under AppOrigin is served.
func (s *Server) appHost(r *http.Request) (string, bool) {
	_, suffix, ok := strings.Cut(s.AppOrigin, "://*")
	if !ok {
		return "", false
	}
	label, ok := strings.CutSuffix(requestHost(r), strings.ToLower(suffix))
	if !ok || strings.Contains(label, ".") {
		return "", false
	}
	if generatedLabelPattern.MatchString(label) {
		return label, true
	}
	_, ok = s.Store.AppByLabel(label)
	return label, ok
}

// checkAppLabel refuses a label for an app's public links that this server cannot give it.
// The store checks its shape and that no other app has it.
func (s *Server) checkAppLabel(label *string) error {
	if label == nil || *label == "" {
		return nil
	}
	if s.AppOrigin == "" {
		return errors.New("app hostnames are off on this server (set SHARTIFACTS_APP_ORIGIN, -app-origin, to turn them on), so public links have no label to set")
	}
	host := strings.ToLower(strings.Replace(strings.SplitN(s.AppOrigin, "://", 2)[1], "*", *label, 1))
	taken := []string{s.ShareOrigin, s.PublicOrigin}
	for _, h := range s.AllowedHosts {
		taken = append(taken, "//"+strings.TrimSpace(h))
	}
	for _, o := range taken {
		if u, err := url.Parse(o); err == nil && strings.EqualFold(u.Hostname(), host) {
			return fmt.Errorf("label %s would make the links' hostname %s, which this server uses for something else", *label, host)
		}
	}
	return nil
}

// fromAppItself reports whether a request to an app's own hostname comes from the app's own
// pages, or is a top-level GET navigation (someone following a link to it). Cookies and
// storage there are the app's, so nothing else may call it: not other sites, and not the
// other apps' hostnames, which browsers count as the same site. Clients that send neither
// Fetch Metadata nor a foreign Origin (curl, servers) act for no visitor, and pass.
func (s *Server) fromAppItself(r *http.Request) bool {
	if (r.Method == "GET" || r.Method == "HEAD") && r.Header.Get("Sec-Fetch-Mode") == "navigate" {
		return true
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return true
	case "":
		origin := r.Header.Get("Origin")
		return origin == "" || origin == s.clientScheme(r)+"://"+r.Host
	}
	return false
}
