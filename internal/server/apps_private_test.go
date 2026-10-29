package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
)

// privateServer is an app server with private app addresses under *.apps.example.com, the
// workspace at shartifacts.example.com, and two apps: Budget, with a backend that echoes
// its cookies, at budget.apps.example.com, and Other, at other.apps.example.com.
func privateServer(t *testing.T) (*Server, http.Handler, string, string) {
	t.Helper()
	s, h := appServer(t, nil)
	web := testWeb()
	web["hub.html"] = &fstest.MapFile{Data: []byte("apps")}
	web["login.html"] = &fstest.MapFile{Data: []byte("login page")}
	web["vendor/lib.js"] = &fstest.MapFile{Data: []byte("library")}
	web["icon-512.png"] = &fstest.MapFile{Data: pngOf(t, 512, 512)}
	web["icon-maskable-512.png"] = &fstest.MapFile{Data: pngOf(t, 512, 512)}
	s.Web = web
	s.PrivateAppOrigin = "https://*.apps.example.com"
	s.PublicOrigin = "https://shartifacts.example.com"
	s.AllowedHosts = []string{"shartifacts.example.com"}
	create := func(name, label string, files map[string]string) string {
		b, _ := json.Marshal(map[string]any{"name": name, "label": label, "files": files})
		created := decodeBody(t, call(h, "POST", "/api/apps", string(b)))
		if d, _ := created["deployment"].(map[string]any); d == nil || d["status"] != "ready" {
			t.Fatalf("deploy %s: %v", name, created)
		}
		return created["app"].(map[string]any)["id"].(string)
	}
	id := create("Budget", "budget", map[string]string{"index.html": "<h1>budget</h1>", "server/main.go": cookieBackend})
	other := create("Other", "other", map[string]string{"index.html": "<h1>other</h1>"})
	return s, h, id, other
}

var visit = []string{"Sec-Fetch-Site", "none", "Sec-Fetch-Mode", "navigate", "Sec-Fetch-Dest", "document"}

// An app's private hostname serves the app at its root to the workspace's people, so it
// installs as an app of its own; nobody else reaches it, nothing but its own pages calls it,
// and nothing of the workspace is served there.
func TestPrivateAppAddress(t *testing.T) {
	s, h, id, other := privateServer(t)
	const home = "https://budget.apps.example.com"

	detail := decodeBody(t, call(h, "GET", "/api/apps/"+id, ""))
	if detail["private_url"] != home+"/" || !strings.HasSuffix(fmt.Sprint(detail["open_url"]), "/apps/"+id+"/open") {
		t.Fatalf("app view: private_url %v, open_url %v", detail["private_url"], detail["open_url"])
	}

	// Its page, at the root, keeps its origin and links a manifest of its own.
	page := call(h, "GET", home+"/", "", visit...)
	body := page.Body.String()
	if page.Code != 200 || !strings.Contains(body, "<h1>budget</h1>") || !strings.Contains(body, `<base href="/">`) || !strings.Contains(body, `<link rel="manifest" href="/_shartifacts/manifest.webmanifest">`) {
		t.Fatalf("page: %d %s", page.Code, body)
	}
	if csp := page.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "allow-same-origin") || !strings.Contains(csp, "manifest-src "+home+"/") {
		t.Fatalf("page CSP: %s", csp)
	}
	if m := sessionInPage.FindStringSubmatch(body); m == nil || m[1] != "" {
		t.Fatalf("a private page carries an app session, which would work elsewhere: %v", m)
	}

	// The manifest makes the root the app: its own id, scope and start, and served icons.
	var m installManifest
	w := call(h, "GET", home+"/_shartifacts/manifest.webmanifest", "", "Sec-Fetch-Site", "same-origin", "Sec-Fetch-Mode", "cors")
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &m) != nil || m.ID != "/" || m.Scope != "/" || m.StartURL != "/" || m.Name != "Budget" || m.Display != "standalone" || len(m.Icons) == 0 {
		t.Fatalf("manifest: %d %s", w.Code, w.Body)
	}
	for _, icon := range m.Icons {
		if !strings.HasPrefix(icon.Src, "/_shartifacts/icon") {
			t.Fatalf("icon %s is not on the private hostname", icon.Src)
		}
		if w := call(h, "GET", home+icon.Src, "", "Sec-Fetch-Site", "same-origin", "Sec-Fetch-Mode", "no-cors"); w.Code != 200 || w.Header().Get("Content-Type") != "image/png" {
			t.Fatalf("icon %s: %d", icon.Src, w.Code)
		}
	}

	// Its backend is at /api/, with the app's cookies passing both ways, kept to the host.
	w = call(h, "GET", home+"/api/cookie", "", "Sec-Fetch-Site", "same-origin", "Cookie", "__Host-sid=abc")
	if w.Code != 200 || w.Body.String() != "cookie:__Host-sid=abc" || len(w.Header().Values("Set-Cookie")) != 2 {
		t.Fatalf("backend: %d %s %v", w.Code, w.Body, w.Header())
	}
	if w := call(h, "POST", home+"/api/cookie", "{}", "Sec-Fetch-Site", "same-origin", "Origin", home); w.Code != 200 {
		t.Fatalf("own page's POST: %d %s", w.Code, w.Body)
	}
	// Libraries and media load by absolute URL from the app's own origin.
	if w := call(h, "GET", home+"/vendor/lib.js", "", "Sec-Fetch-Site", "same-origin", "Sec-Fetch-Mode", "no-cors"); w.Code != 200 || w.Body.String() != "library" {
		t.Fatalf("vendor: %d %s", w.Code, w.Body)
	}

	// Nothing else calls it: not other sites, not other apps' hostnames (same-site).
	for _, hs := range [][]string{
		{"Sec-Fetch-Site", "same-site", "Sec-Fetch-Mode", "cors"},
		{"Sec-Fetch-Site", "cross-site", "Sec-Fetch-Mode", "no-cors"},
		{"Origin", "https://evil.example"},
		{"Origin", "null"},
		{"Origin", "https://other.apps.example.com"},
		{"Origin", "https://shartifacts.example.com"},
		{"Origin", "http://budget.apps.example.com"},
	} {
		if w := call(h, "POST", home+"/api/cookie", "{}", hs...); w.Code != 403 {
			t.Fatalf("%v reached the backend: %d", hs, w.Code)
		}
	}
	if w := call(h, "POST", home+"/api/cookie", "{}", "Sec-Fetch-Site", "cross-site", "Sec-Fetch-Mode", "navigate", "Sec-Fetch-Dest", "document"); w.Code != 403 {
		t.Fatalf("cross-site form post: %d", w.Code)
	}

	// Only the workspace's people: not the internet, directly or through a proxy.
	from := func(peer string, headers ...string) int {
		r := httptest.NewRequest("GET", home+"/", nil)
		r.RemoteAddr = peer
		for i := 0; i+1 < len(headers); i += 2 {
			r.Header.Set(headers[i], headers[i+1])
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	if code := from("203.0.113.7:4000"); code != 403 {
		t.Fatalf("a public peer: %d", code)
	}
	if code := from("192.168.1.9:4000", "X-Forwarded-For", "203.0.113.7"); code != 403 {
		t.Fatalf("a proxy forwarding a public client: %d", code)
	}
	if code := from("100.100.1.2:4000"); code != 200 {
		t.Fatalf("a tailnet peer: %d", code)
	}
	// The public listener answers it as the share hostname, which has no such page.
	r := httptest.NewRequest("GET", home+"/", nil)
	r.RemoteAddr = "203.0.113.7:4000"
	rec := httptest.NewRecorder()
	s.PublicHandler().ServeHTTP(rec, r)
	if rec.Code != 404 || strings.Contains(rec.Body.String(), "budget") {
		t.Fatalf("the public listener served a private address: %d %s", rec.Code, rec.Body)
	}

	// Each hostname is its app's alone, and nothing of the workspace.
	if w := call(h, "GET", "https://other.apps.example.com/", "", visit...); w.Code != 200 || !strings.Contains(w.Body.String(), "<h1>other</h1>") {
		t.Fatalf("other app: %d %s", w.Code, w.Body)
	}
	for _, p := range []string{"/hub.js", "/app.js", "/api/tls-allowed?domain=share.example.com", "/_shartifacts/nothing"} {
		if w := call(h, "GET", home+p, "", "Sec-Fetch-Site", "same-origin"); w.Code != 404 {
			t.Fatalf("private hostname served %s: %d %s", p, w.Code, w.Body)
		}
	}
	// The workspace's routes are the app's paths there, which its client-side routing gets.
	for _, p := range []string{"/apps/" + id + "/open", "/run/" + id + "/live/", "/login"} {
		if w := call(h, "GET", home+p, "", visit...); w.Code != 200 || !strings.Contains(w.Body.String(), "<h1>budget</h1>") {
			t.Fatalf("private hostname's %s: %d %v", p, w.Code, w.Header())
		}
	}
	if w := call(h, "GET", home+"/mcp", "", "Sec-Fetch-Site", "same-origin"); strings.Contains(w.Body.String(), "jsonrpc") {
		t.Fatalf("private hostname reached MCP: %s", w.Body)
	}
	if w := call(h, "GET", "https://nobody.apps.example.com/", "", visit...); w.Code != 404 {
		t.Fatalf("a label no app has: %d", w.Code)
	}
	if w := call(h, "GET", "https://x.budget.apps.example.com/", "", visit...); w.Code != 403 {
		t.Fatalf("a deeper name: %d", w.Code)
	}

	// Its generated label, and a label it had, send people to the one it has now.
	gen := "https://" + s.generatedLabel(id) + ".apps.example.com"
	if w := call(h, "GET", gen+"/notes?x=1", "", visit...); w.Code != 302 || w.Header().Get("Location") != home+"/notes?x=1" {
		t.Fatalf("generated label: %d %v", w.Code, w.Header())
	}
	if w := call(h, "GET", gen+"/api/cookie", "", "Sec-Fetch-Site", "same-origin", "Sec-Fetch-Mode", "cors"); w.Code != 404 {
		t.Fatalf("generated label's API: %d", w.Code)
	}
	call(h, "PATCH", "/api/apps/"+other, `{"label":"renamed"}`)
	if w := call(h, "GET", "https://other.apps.example.com/", "", visit...); w.Code != 302 || w.Header().Get("Location") != "https://renamed.apps.example.com/" {
		t.Fatalf("former label: %d %v", w.Code, w.Header())
	}
	// A label that would make a private hostname one of the workspace's names is refused.
	s.AllowedHosts = append(s.AllowedHosts, "box.apps.example.com")
	if w := call(h, "PATCH", "/api/apps/"+other, `{"label":"box"}`); w.Code != 400 {
		t.Fatalf("label on a workspace name: %d %s", w.Code, w.Body)
	}
}

// With sign-in on, a private hostname asks for a sign-in of its own, which comes from the
// workspace's: a one-time ticket swapped for a cookie bound to the workspace session.
func TestPrivateAppSignIn(t *testing.T) {
	s, h, id, other := privateServer(t)
	const home = "https://budget.apps.example.com"
	const workspace = "https://shartifacts.example.com"
	live := sessionInPage.FindStringSubmatch(call(h, "GET", "/run/"+id+"/live/", "").Body.String())[1]
	if err := s.Auth.SetPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	session, err := s.Auth.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	owner := secureSessionCookie + "=" + session

	// Without it, a page sends the browser to the workspace, which sends it back signed in;
	// anything else is refused.
	if w := call(h, "GET", home+"/notes?x=1", "", visit...); w.Code != 303 || w.Header().Get("Location") != workspace+"/apps/"+id+"/open?next="+url.QueryEscape("/notes?x=1") {
		t.Fatalf("unsigned page: %d %v", w.Code, w.Header())
	}
	for _, p := range []string{"/api/cookie", "/_shartifacts/manifest.webmanifest", "/vendor/lib.js"} {
		if w := call(h, "GET", home+p, "", "Sec-Fetch-Site", "same-origin", "Sec-Fetch-Mode", "cors"); w.Code != 401 {
			t.Fatalf("unsigned %s: %d", p, w.Code)
		}
	}
	if w := call(h, "GET", workspace+"/apps/"+id+"/open", "", visit...); w.Code != 303 || !strings.HasPrefix(w.Header().Get("Location"), "/login?next=") {
		t.Fatalf("open without the workspace's sign-in: %d %v", w.Code, w.Header())
	}

	ticket := func(app, next string) string {
		w := call(h, "GET", workspace+"/apps/"+app+"/open?next="+url.QueryEscape(next), "", append(visit, "Cookie", owner)...)
		loc, err := url.Parse(w.Header().Get("Location"))
		if w.Code != 303 || err != nil || loc.Path != "/_shartifacts/enter" || loc.Query().Get("t") == "" {
			t.Fatalf("open: %d %v", w.Code, w.Header())
		}
		return loc.Scheme + "://" + loc.Host + loc.RequestURI()
	}
	enter := ticket(id, "/notes?x=1")
	if !strings.HasPrefix(enter, home+"/_shartifacts/enter?") {
		t.Fatalf("ticket sent to %s", enter)
	}
	w := call(h, "GET", enter, "", "Sec-Fetch-Site", "cross-site", "Sec-Fetch-Mode", "navigate", "Sec-Fetch-Dest", "document")
	if w.Code != 303 || w.Header().Get("Location") != "/notes?x=1" {
		t.Fatalf("enter: %d %v %s", w.Code, w.Header(), w.Body)
	}
	var cookie string
	for _, c := range w.Result().Cookies() {
		if c.Name == securePrivateCookie {
			if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Domain != "" {
				t.Fatalf("cookie attributes: %+v", c)
			}
			cookie = c.Name + "=" + c.Value
		}
	}
	if cookie == "" {
		t.Fatalf("no cookie: %v", w.Header())
	}
	// A ticket works once.
	if w := call(h, "GET", enter, "", visit...); w.Code != 403 || len(w.Result().Cookies()) != 0 {
		t.Fatalf("a used ticket: %d", w.Code)
	}

	// Signed in, the page and its backend answer, and the backend never sees the cookie.
	if w := call(h, "GET", home+"/", "", append(visit, "Cookie", cookie)...); w.Code != 200 || !strings.Contains(w.Body.String(), "<h1>budget</h1>") {
		t.Fatalf("signed-in page: %d %s", w.Code, w.Body)
	}
	w = call(h, "GET", home+"/api/cookie", "", "Sec-Fetch-Site", "same-origin", "Cookie", "a=1; "+cookie+"; b=2")
	if w.Code != 200 || w.Body.String() != "cookie:a=1; b=2" {
		t.Fatalf("backend saw: %d %s", w.Code, w.Body)
	}

	// Neither a ticket nor a cookie stands in for another app's, or for each other, and
	// neither does a page's app session from the workspace.
	if w := call(h, "GET", strings.Replace(ticket(other, "/"), "other.apps", "budget.apps", 1), "", visit...); w.Code != 403 {
		t.Fatalf("another app's ticket: %d", w.Code)
	}
	if w := call(h, "GET", "https://other.apps.example.com/", "", append(visit, "Cookie", cookie)...); w.Code != 303 {
		t.Fatalf("another app's cookie: %d", w.Code)
	}
	bare := strings.TrimPrefix(cookie, securePrivateCookie+"=")
	if w := call(h, "GET", home+"/_shartifacts/enter?t="+url.QueryEscape(bare), "", visit...); w.Code != 403 {
		t.Fatalf("a cookie as a ticket: %d", w.Code)
	}
	fresh := ticket(id, "/")
	tv, _ := url.Parse(fresh)
	for _, forged := range []string{securePrivateCookie + "=" + tv.Query().Get("t"), securePrivateCookie + "=" + live, privateCookie + "=garbage"} {
		if w := call(h, "GET", home+"/api/cookie", "", "Sec-Fetch-Site", "same-origin", "Cookie", forged); w.Code != 401 {
			t.Fatalf("%s as the cookie: %d", forged, w.Code)
		}
	}
	// A next that leaves the hostname goes to its root instead.
	if w := call(h, "GET", strings.Replace(fresh, "next=%2F", "next="+url.QueryEscape("//evil.example/"), 1), "", visit...); w.Code != 303 || w.Header().Get("Location") != "/" {
		t.Fatalf("enter with an outside next: %d %v", w.Code, w.Header())
	}

	// Signing out of the workspace ends it.
	if err := s.Auth.EndSession(session); err != nil {
		t.Fatal(err)
	}
	if w := call(h, "GET", home+"/api/cookie", "", "Sec-Fetch-Site", "same-origin", "Cookie", cookie); w.Code != 401 {
		t.Fatalf("cookie after signing out: %d", w.Code)
	}
}

func TestWithoutCookies(t *testing.T) {
	h := http.Header{}
	h.Add("Cookie", "a=1; shartifacts_app=x;b=2")
	h.Add("Cookie", "__Host-shartifacts_app=y")
	withoutCookies(h, privateCookie, securePrivateCookie)
	if got := h.Values("Cookie"); len(got) != 1 || got[0] != "a=1; b=2" {
		t.Fatalf("%q", got)
	}
	withoutCookies(h, "a", "b")
	if got := h.Values("Cookie"); len(got) != 0 {
		t.Fatalf("%q", got)
	}
}
