package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/LaneBirmingham/shartifacts/internal/auth"
	"github.com/LaneBirmingham/shartifacts/internal/runner"
	"github.com/LaneBirmingham/shartifacts/internal/store"
)

// Hashing at the production work factor would make every sign-in test slow.
func init() { auth.Iterations = 1000 }

func signInServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	web := testWeb()
	web["login.html"] = &fstest.MapFile{Data: []byte("login page")}
	web["hub.html"] = &fstest.MapFile{Data: []byte("apps")}
	s := &Server{Store: st, Web: web, ShareOrigin: "https://share.example.com", AllowedHosts: []string{"shartifacts.example.com"}, PublicOrigin: "https://shartifacts.example.com"}
	return s, s.Handler()
}

// cookieOf is the session cookie a response set, as a request header value.
func cookieOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
				t.Fatalf("session cookie attributes: %+v", c)
			}
			return c.Name + "=" + c.Value
		}
	}
	t.Fatalf("no session cookie: %d %s", w.Code, w.Body)
	return ""
}

func TestSignInOffChangesNothing(t *testing.T) {
	_, h := signInServer(t)
	if w := call(h, "GET", "/api/artifacts", ""); w.Code != 200 {
		t.Fatalf("API with sign-in off: %d", w.Code)
	}
	if b := decodeBody(t, call(h, "GET", "/api/auth", "")); b["enabled"] != false || b["signed_in"] != false {
		t.Fatal(b)
	}
	if w := call(h, "GET", "/login", ""); w.Code != 303 || w.Header().Get("Location") != "/" {
		t.Fatalf("login page with sign-in off: %d", w.Code)
	}
	// Agents and browsers alike have nothing to manage yet.
	for _, path := range []string{"/api/auth/tokens"} {
		if w := call(h, "GET", path, ""); w.Code != 409 {
			t.Fatalf("%s with sign-in off: %d", path, w.Code)
		}
	}
	if w := call(h, "POST", "/api/auth/login", `{"password":"anything at all"}`); w.Code != 409 {
		t.Fatalf("login with sign-in off: %d", w.Code)
	}
}

func TestSignInGate(t *testing.T) {
	s, h := signInServer(t)
	a, err := s.Store.Save("", store.Input{Title: "Plan", Kind: "markdown", Content: "# Plan"})
	if err != nil {
		t.Fatal(err)
	}
	// Whoever can use the workspace can set the first password, and stays signed in.
	if w := call(h, "PUT", "/api/auth/password", `{"password":"short"}`); w.Code != 400 {
		t.Fatalf("short password: %d", w.Code)
	}
	w := call(h, "PUT", "/api/auth/password", `{"password":"correct horse"}`)
	if w.Code != 200 {
		t.Fatalf("set password: %d %s", w.Code, w.Body)
	}
	owner := cookieOf(t, w)

	// Refused without sign-in: pages go to the login page, the API and MCP answer 401.
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/artifacts"}, {"GET", "/api/artifacts/" + a.ID}, {"DELETE", "/api/artifacts/" + a.ID},
		{"GET", "/api/media"}, {"POST", "/api/media"}, {"GET", "/api/apps"}, {"GET", "/api/search?q=plan"},
		{"POST", "/mcp"}, {"GET", "/api/auth/tokens"}, {"POST", "/api/auth/tokens"}, {"PUT", "/api/auth/password"},
		{"DELETE", "/api/auth/password"}, {"GET", "/api/config"}, {"GET", "/hub.html"}, {"GET", "/index.html"},
		{"GET", "/no-such-route"}, {"POST", "/api/artifacts/" + a.ID + "/comments"},
	} {
		w := call(h, tc.method, tc.path, `{}`)
		if w.Code != 401 || !strings.Contains(w.Body.String(), "unauthenticated") || w.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s %s without sign-in: %d %s", tc.method, tc.path, w.Code, w.Body)
		}
	}
	for _, p := range []string{"/", "/apps", "/artifacts", "/a/" + a.ID, "/view/" + a.ID + "?version=1"} {
		w := call(h, "GET", p, "")
		if w.Code != 303 || w.Header().Get("Location") != "/login?next="+strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(p, "/", "%2F"), "?", "%3F"), "=", "%3D") {
			t.Errorf("page %s: %d %s", p, w.Code, w.Header().Get("Location"))
		}
	}
	// Open without sign-in: what previews load cross-site, share links, the login page and
	// the UI's own files. None of these list or change anything.
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/healthz", 200}, {"GET", "/login", 200}, {"GET", "/api/auth", 200}, {"GET", "/app.js", 200},
		{"GET", "/preview/" + a.ID, 200}, {"GET", "/media/0123456789abcdef0123456789abcdef.mp4", 404},
		{"GET", "/s/nosuchtoken", 404}, {"GET", "/.well-known/oauth-protected-resource", 404},
		{"GET", "/run/0123456789abcdef01234567/live/", 404}, {"GET", "/p/nosuchtoken/", 404},
	} {
		if w := call(h, tc.method, tc.path, ""); w.Code != tc.want {
			t.Errorf("%s %s without sign-in: %d, want %d", tc.method, tc.path, w.Code, tc.want)
		}
	}

	// With the cookie the browser that set the password got, everything works.
	if w := call(h, "GET", "/api/artifacts", "", "Cookie", owner); w.Code != 200 {
		t.Fatalf("API signed in: %d", w.Code)
	}
	if w := call(h, "GET", "/", "", "Cookie", owner); w.Code != 200 || w.Body.String() != "apps" {
		t.Fatalf("page signed in: %d", w.Code)
	}
	if w := call(h, "GET", "/login?next=%2Fa%2F"+a.ID, "", "Cookie", owner); w.Code != 303 || w.Header().Get("Location") != "/a/"+a.ID {
		t.Fatalf("login page signed in: %d %s", w.Code, w.Header().Get("Location"))
	}
	for _, next := range []string{"//evil.example/", "/%09/evil.example", "/%5Cevil.example", "https%3A%2F%2Fevil.example%2F"} {
		if w := call(h, "GET", "/login?next="+next, "", "Cookie", owner); w.Header().Get("Location") != "/" {
			t.Errorf("open redirect through %s: %q", next, w.Header().Get("Location"))
		}
	}
	// Encoded dot segments cannot borrow an open prefix to reach the rest, and the UI's
	// pages are served only by their routes.
	for _, p := range []string{"/s/%2e%2e/hub.html", "/vendor/%2e%2e/api/artifacts", "/.well-known/%2e%2e/api/config", "/media/%2E%2E/index.html", "/run/%2e%2e/api/apps"} {
		if w := call(h, "GET", p, ""); w.Code != 400 {
			t.Errorf("%s without sign-in: %d", p, w.Code)
		}
	}
	if w := call(h, "GET", "/hub.html", "", "Cookie", owner); w.Code != 404 {
		t.Fatalf("a UI page by its file name: %d", w.Code)
	}
	if b := decodeBody(t, call(h, "GET", "/api/auth", "", "Cookie", owner)); b["enabled"] != true || b["signed_in"] != true || b["via"] != "session" {
		t.Fatal(b)
	}
	// A cookie that is not a session counts for nothing.
	if w := call(h, "GET", "/api/artifacts", "", "Cookie", sessionCookie+"=forged"); w.Code != 401 {
		t.Fatalf("forged cookie: %d", w.Code)
	}
	// The network checks still come first: a signed-in cookie does not open the workspace
	// to a public address or a foreign site.
	r := httptest.NewRequest("GET", "http://127.0.0.1:8080/api/artifacts", nil)
	r.RemoteAddr = "8.8.8.8:1"
	r.Header.Set("Cookie", owner)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 403 {
		t.Fatalf("public peer with a cookie: %d", rec.Code)
	}
	if w := call(h, "POST", "/api/artifacts", `{}`, "Cookie", owner, "Origin", "https://evil.example"); w.Code != 403 {
		t.Fatalf("cross-origin with a cookie: %d", w.Code)
	}
	// Share links on the share hostname never ask.
	if w := call(h, "GET", "https://share.example.com/api/artifacts", ""); w.Code != 404 {
		t.Fatalf("workspace on the share host: %d", w.Code)
	}
}

func TestSignInAndOut(t *testing.T) {
	_, h := signInServer(t)
	first := cookieOf(t, call(h, "PUT", "/api/auth/password", `{"password":"correct horse"}`))
	if w := call(h, "POST", "/api/auth/login", `{"password":"wrong horse"}`); w.Code != 403 || !strings.Contains(w.Body.String(), "wrong_password") {
		t.Fatalf("wrong password: %d %s", w.Code, w.Body)
	}
	w := call(h, "POST", "/api/auth/login", `{"password":"correct horse"}`)
	if w.Code != 200 {
		t.Fatalf("sign in: %d %s", w.Code, w.Body)
	}
	second := cookieOf(t, w)
	// Over HTTPS the cookie is Secure, with the __Host- prefix so that no sibling hostname
	// can set one in its place.
	https := call(h, "POST", "https://shartifacts.example.com/api/auth/login", `{"password":"correct horse"}`, "Origin", "https://shartifacts.example.com")
	c := https.Result().Cookies()
	if len(c) != 1 || !c[0].Secure || c[0].Name != secureSessionCookie || c[0].Domain != "" {
		t.Fatalf("cookie over https: %d %+v", https.Code, c)
	}
	if w := call(h, "GET", "https://shartifacts.example.com/api/artifacts", "", "Cookie", c[0].Name+"="+c[0].Value); w.Code != 200 {
		t.Fatalf("signed in over https: %d", w.Code)
	}
	// A cookie of the same name set for a parent domain, sent first, does not hide the real one.
	if w := call(h, "GET", "/api/artifacts", "", "Cookie", sessionCookie+"=planted; "+second); w.Code != 200 {
		t.Fatalf("a planted cookie hid the session: %d", w.Code)
	}

	// Signing out ends that session only.
	if w := call(h, "POST", "/api/auth/logout", "", "Cookie", second); w.Code != 200 {
		t.Fatalf("sign out: %d", w.Code)
	}
	if w := call(h, "GET", "/api/artifacts", "", "Cookie", second); w.Code != 401 {
		t.Fatalf("after sign-out: %d", w.Code)
	}
	if w := call(h, "GET", "/api/artifacts", "", "Cookie", first); w.Code != 200 {
		t.Fatalf("another browser was signed out: %d", w.Code)
	}

	// Changing the password needs the current one, and signs every other browser out.
	third := cookieOf(t, call(h, "POST", "/api/auth/login", `{"password":"correct horse"}`))
	if w := call(h, "PUT", "/api/auth/password", `{"current":"nope nope","password":"battery staple"}`, "Cookie", first); w.Code != 403 {
		t.Fatalf("change with a wrong current password: %d", w.Code)
	}
	w = call(h, "PUT", "/api/auth/password", `{"current":"correct horse","password":"battery staple"}`, "Cookie", first)
	if w.Code != 200 {
		t.Fatalf("change password: %d %s", w.Code, w.Body)
	}
	fourth := cookieOf(t, w)
	if call(h, "GET", "/api/artifacts", "", "Cookie", third).Code != 401 || call(h, "GET", "/api/artifacts", "", "Cookie", fourth).Code != 200 {
		t.Fatal("a password change must sign other browsers out and keep this one in")
	}
	if call(h, "POST", "/api/auth/login", `{"password":"correct horse"}`).Code != 403 || call(h, "POST", "/api/auth/login", `{"password":"battery staple"}`).Code != 200 {
		t.Fatal("old password still works, or the new one does not")
	}

	// Sign out everywhere.
	fifth := cookieOf(t, call(h, "POST", "/api/auth/login", `{"password":"battery staple"}`))
	if w := call(h, "POST", "/api/auth/logout", `{"everywhere":true}`, "Cookie", fourth); w.Code != 200 {
		t.Fatalf("sign out everywhere: %d", w.Code)
	}
	if call(h, "GET", "/api/artifacts", "", "Cookie", fifth).Code != 401 {
		t.Fatal("a browser survived signing out everywhere")
	}

	// Turning sign-in off needs the current password too, and opens the workspace again.
	sixth := cookieOf(t, call(h, "POST", "/api/auth/login", `{"password":"battery staple"}`))
	if w := call(h, "DELETE", "/api/auth/password", `{"current":"correct horse"}`, "Cookie", sixth); w.Code != 403 {
		t.Fatalf("turn off with the old password: %d", w.Code)
	}
	if w := call(h, "DELETE", "/api/auth/password", `{"current":"battery staple"}`, "Cookie", sixth); w.Code != 200 {
		t.Fatalf("turn off: %d %s", w.Code, w.Body)
	}
	if w := call(h, "GET", "/api/artifacts", ""); w.Code != 200 {
		t.Fatalf("API after turning sign-in off: %d", w.Code)
	}
}

// login tries a password from peer, through a proxy that reports client when it is set.
func login(h http.Handler, peer, client, pw string) int {
	r := httptest.NewRequest("POST", "http://127.0.0.1:8080/api/auth/login", strings.NewReader(`{"password":"`+pw+`"}`))
	r.RemoteAddr = peer + ":1"
	r.Header.Set("Content-Type", "application/json")
	if client != "" {
		r.Header.Set("X-Forwarded-For", client)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Code
}

func TestWrongPasswordsAreThrottled(t *testing.T) {
	s, h := signInServer(t)
	call(h, "PUT", "/api/auth/password", `{"password":"correct horse"}`)
	for i := 0; i < attemptLimit; i++ {
		if w := call(h, "POST", "/api/auth/login", `{"password":"guess guess"}`); w.Code != 403 {
			t.Fatalf("guess %d: %d", i, w.Code)
		}
	}
	w := call(h, "POST", "/api/auth/login", `{"password":"correct horse"}`)
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("after %d wrong passwords, the right one: %d", attemptLimit, w.Code)
	}
	// Another address is not held up.
	r := httptest.NewRequest("POST", "http://127.0.0.1:8080/api/auth/login", strings.NewReader(`{"password":"correct horse"}`))
	r.RemoteAddr = "192.168.1.10:1"
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("another address: %d", rec.Code)
	}

	// Guesses sent all at once are counted before they are hashed, so no more than the limit
	// are checked.
	s.attempts = passwordAttempts{}
	codes := make(chan int, 4*attemptLimit)
	var wg sync.WaitGroup
	for range 4 * attemptLimit {
		wg.Go(func() { codes <- login(h, "192.168.1.11", "", "guess guess") })
	}
	wg.Wait()
	close(codes)
	count := map[int]int{}
	for c := range codes {
		count[c]++
	}
	if count[403] != attemptLimit || count[429] != 3*attemptLimit {
		t.Fatalf("a burst of guesses: %v", count)
	}

	// Behind a proxy, clients are told apart by the address it adds last, so one guesser
	// does not lock everyone else out; writing X-Forwarded-For yourself does not help,
	// because the proxy adds the real address after it.
	proxy := "127.0.0.1"
	for range attemptLimit {
		login(h, proxy, "100.64.0.5", "guess guess")
	}
	if code := login(h, proxy, "100.64.0.5", "correct horse"); code != 429 {
		t.Fatalf("the guesser behind the proxy: %d", code)
	}
	if code := login(h, proxy, "100.64.0.9, 100.64.0.5", "correct horse"); code != 429 {
		t.Fatalf("the guesser, naming someone else first: %d", code)
	}
	if code := login(h, proxy, "100.64.0.6", "correct horse"); code != 200 {
		t.Fatalf("someone else behind the proxy: %d", code)
	}
	// A guesser who varies the address still meets the proxy's limit as a whole.
	for i := range peerLimit {
		login(h, proxy, fmt.Sprintf("100.64.1.%d", i), "guess guess")
	}
	if code := login(h, proxy, "100.64.0.7", "correct horse"); code != 429 {
		t.Fatalf("after %d guesses through one proxy: %d", peerLimit, code)
	}
}

func TestSafeNext(t *testing.T) {
	for next, want := range map[string]string{
		"/a/0123?version=2#top": "/a/0123?version=2#top", "/": "/", "/apps/x%20y": "/apps/x%20y",
		"": "/", "a/b": "/", "//evil.example": "/", "/\\evil.example": "/", "/\t/evil.example": "/",
		"/\n/evil.example": "/", "/ /evil.example": "/", "https://evil.example/": "/", "/caf\u00e9": "/",
	} {
		if got := safeNext(next); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", next, got, want)
		}
	}
}

func TestWorkspaceToken(t *testing.T) {
	h := http.Header{}
	h.Add("Authorization", "Bearer app-secret")
	if workspaceToken(h) {
		t.Fatal("an app's own Authorization taken for a workspace token")
	}
	h.Add("Authorization", "bearer "+auth.TokenPrefix+"abc")
	if !workspaceToken(h) {
		t.Fatal("a workspace token in a second Authorization header was missed")
	}
}

// Setting the first password never replaces one set meanwhile, for example with the CLI
// while this server still thinks sign-in is off.
func TestFirstPasswordIsSetOnce(t *testing.T) {
	s, h := signInServer(t)
	cli, err := auth.Open(s.Store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if err := cli.SetPassword("set on the server"); err != nil {
		t.Fatal(err)
	}
	if w := call(h, "PUT", "/api/auth/password", `{"password":"someone else's"}`); w.Code == 200 {
		t.Fatal("a second first password replaced the first")
	}
	if !s.Auth.CheckPassword("set on the server") {
		t.Fatal("the CLI's password was lost")
	}
}

func TestAgentTokens(t *testing.T) {
	_, h := signInServer(t)
	owner := cookieOf(t, call(h, "PUT", "/api/auth/password", `{"password":"correct horse"}`))
	w := call(h, "POST", "/api/auth/tokens", `{"name":"Claude Code"}`, "Cookie", owner)
	if w.Code != 201 {
		t.Fatalf("make token: %d %s", w.Code, w.Body)
	}
	made := decodeBody(t, w)
	token, id := made["token"].(string), made["id"].(string)
	bearer := "Bearer " + token

	if w := call(h, "GET", "/api/artifacts", "", "Authorization", bearer); w.Code != 200 {
		t.Fatalf("API with a token: %d", w.Code)
	}
	if w := call(h, "GET", "/api/artifacts", "", "Authorization", "Bearer shx_wrong"); w.Code != 401 || !strings.Contains(w.Body.String(), "agent token") {
		t.Fatalf("API with a wrong token: %d %s", w.Code, w.Body)
	}
	if b := decodeBody(t, call(h, "GET", "/api/auth", "", "Authorization", bearer)); b["via"] != "token" {
		t.Fatal(b)
	}
	// An agent token cannot manage sign-in: only people can.
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/api/auth/tokens", ""}, {"POST", "/api/auth/tokens", `{"name":"more"}`},
		{"DELETE", "/api/auth/tokens/" + id, ""}, {"PUT", "/api/auth/password", `{"current":"correct horse","password":"battery staple"}`},
		{"DELETE", "/api/auth/password", `{"current":"correct horse"}`}, {"POST", "/api/auth/logout", `{"everywhere":true}`},
	} {
		if w := call(h, tc.method, tc.path, tc.body, "Authorization", bearer); w.Code != 403 {
			t.Errorf("%s %s with a token: %d", tc.method, tc.path, w.Code)
		}
	}

	// MCP: without a token, a 401 that says what to do; with one, the tools, and upload
	// URLs the shell can use without the token.
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`
	if w := call(h, "POST", "/mcp", init, "Accept", "application/json, text/event-stream"); w.Code != 401 || !strings.Contains(w.Header().Get("WWW-Authenticate"), "Bearer") || !strings.Contains(w.Body.String(), "agent token") {
		t.Fatalf("MCP without a token: %d %s", w.Code, w.Body)
	}
	list := call(h, "POST", "/mcp", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_media","arguments":{}}}`, "Accept", "application/json, text/event-stream", "Authorization", bearer)
	if list.Code != 200 {
		t.Fatalf("MCP with a token: %d %s", list.Code, list.Body)
	}
	upload := regexp.MustCompile(`"upload_url":"http://127\.0\.0\.1:8080(/api/media/[^"]+)"`).FindStringSubmatch(list.Body.String())
	if upload == nil {
		t.Fatalf("no upload grant in %s", list.Body)
	}
	up := httptest.NewRequest("POST", "http://127.0.0.1:8080"+upload[1]+"?name=notes.txt", strings.NewReader("hello"))
	up.RemoteAddr = "192.168.1.9:1"
	up.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, up)
	if rec.Code != 201 {
		t.Fatalf("upload with the grant: %d %s", rec.Code, rec.Body)
	}
	tampered := upload[1][:len(upload[1])-2] + "xx"
	if w := call(h, "POST", tampered, "hello", "Content-Type", "text/plain"); w.Code != 403 {
		t.Fatalf("tampered grant: %d", w.Code)
	}

	// Revoking the token refuses it, and the upload URLs it was given, at once.
	if w := call(h, "DELETE", "/api/auth/tokens/"+id, "", "Cookie", owner); w.Code != 200 {
		t.Fatalf("revoke: %d", w.Code)
	}
	if w := call(h, "GET", "/api/artifacts", "", "Authorization", bearer); w.Code != 401 {
		t.Fatalf("revoked token: %d", w.Code)
	}
	if w := call(h, "POST", upload[1], "hello", "Content-Type", "text/plain"); w.Code != 403 {
		t.Fatalf("revoked token's upload URL: %d", w.Code)
	}

	// An upload URL given to a signed-in browser's MCP request ends when it signs out.
	browser := cookieOf(t, call(h, "POST", "/api/auth/login", `{"password":"correct horse"}`))
	list = call(h, "POST", "/mcp", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_media","arguments":{}}}`, "Accept", "application/json, text/event-stream", "Cookie", browser)
	upload = regexp.MustCompile(`"upload_url":"http://127\.0\.0\.1:8080(/api/media/[^"]+)"`).FindStringSubmatch(list.Body.String())
	if upload == nil {
		t.Fatalf("no upload grant for a browser: %s", list.Body)
	}
	if w := call(h, "POST", upload[1]+"?name=a.txt", "hello", "Content-Type", "text/plain"); w.Code != 201 {
		t.Fatalf("a browser's upload URL: %d %s", w.Code, w.Body)
	}
	call(h, "POST", "/api/auth/logout", "", "Cookie", browser)
	if w := call(h, "POST", upload[1]+"?name=b.txt", "hello", "Content-Type", "text/plain"); w.Code != 403 {
		t.Fatalf("a signed-out browser's upload URL: %d", w.Code)
	}

	// Changing the password can revoke every token too, for when someone else knew it.
	kept := decodeBody(t, call(h, "POST", "/api/auth/tokens", `{"name":"kept"}`, "Cookie", owner))["token"].(string)
	w = call(h, "PUT", "/api/auth/password", `{"current":"correct horse","password":"battery staple"}`, "Cookie", owner)
	owner = cookieOf(t, w)
	if call(h, "GET", "/api/artifacts", "", "Authorization", "Bearer "+kept).Code != 200 {
		t.Fatal("a plain password change revoked a token")
	}
	if w := call(h, "PUT", "/api/auth/password", `{"current":"battery staple","password":"correct horse","revoke_tokens":true}`, "Cookie", owner); w.Code != 200 {
		t.Fatalf("change and revoke: %d %s", w.Code, w.Body)
	}
	if call(h, "GET", "/api/artifacts", "", "Authorization", "Bearer "+kept).Code != 401 {
		t.Fatal("revoke_tokens kept a token")
	}
}

// headerRecorder notes the Authorization header each backend request arrives with.
type headerRecorder struct {
	runner.Runtime
	mu   sync.Mutex
	seen []string
}

func (h *headerRecorder) Proxy(w http.ResponseWriter, r *http.Request, app string, version int) {
	h.mu.Lock()
	h.seen = append(h.seen, r.Header.Get("Authorization"))
	h.mu.Unlock()
	h.Runtime.Proxy(w, r, app, version)
}

func (h *headerRecorder) last() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seen[len(h.seen)-1]
}

// With sign-in on, an app page loaded without it carries no app session, and the app's
// backend answers only signed-in requests or a page's own session, which ends with the
// sign-in it was served under.
func TestSignInGatesAppBackends(t *testing.T) {
	rec := &headerRecorder{}
	s, h := appServer(t, func(st *store.Store) runner.Runtime {
		local := localRunner(st.AppsDir(), false)
		t.Cleanup(local.Close)
		rec.Runtime = local
		return rec
	})
	created := decodeBody(t, call(h, "POST", "/api/apps", `{"name":"Notes","template":"notes"}`))
	id := created["app"].(map[string]any)["id"].(string)
	live := "/run/" + id + "/live/"
	// A page loaded while sign-in is off, by anyone on the network, gets a session that
	// stops working once a password is set.
	before := sessionInPage.FindStringSubmatch(call(h, "GET", live, "").Body.String())[1]
	if w := call(h, "GET", live+"api/notes", "", "Origin", "null", SessionHeader, before); w.Code != 200 {
		t.Fatalf("an app session with sign-in off: %d", w.Code)
	}
	if err := s.Auth.SetPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	if w := call(h, "GET", live+"api/notes", "", "Origin", "null", SessionHeader, before); w.Code != 403 {
		t.Fatalf("an app session from before sign-in: %d", w.Code)
	}
	session, err := s.Auth.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	owner := sessionCookie + "=" + session

	// The hub's frame loads the page with the cookie: it gets a session.
	page := call(h, "GET", live, "", "Cookie", owner).Body.String()
	appSession := sessionInPage.FindStringSubmatch(page)[1]
	if appSession == "" {
		t.Fatal("a signed-in page got no app session")
	}
	// Loaded without it (its own frame navigating, or someone who only knows the id), the
	// page still renders but carries none.
	if bare := call(h, "GET", live, ""); bare.Code != 200 || sessionInPage.FindStringSubmatch(bare.Body.String())[1] != "" {
		t.Fatalf("an unsigned page: %d, session %q", bare.Code, sessionInPage.FindStringSubmatch(bare.Body.String())[1])
	}
	for _, tc := range []struct {
		name    string
		headers []string
		want    int
	}{
		{"curl without sign-in", nil, 401},
		{"curl with the cookie", []string{"Cookie", owner}, 200},
		{"curl with the page's session", []string{SessionHeader, appSession}, 200},
		{"sandboxed page with its session", []string{"Origin", "null", SessionHeader, appSession}, 200},
		{"sandboxed page without one", []string{"Origin", "null"}, 403},
	} {
		if w := call(h, "GET", live+"api/notes", "", tc.headers...); w.Code != tc.want {
			t.Errorf("%s: %d, want %d (%s)", tc.name, w.Code, tc.want, w.Body)
		}
	}

	// An agent calls the backend with its token, which signs it in but never reaches the
	// backend. A revoked token is refused. The app's own Authorization header passes.
	token, _, err := s.Auth.NewToken("agent")
	if err != nil {
		t.Fatal(err)
	}
	if w := call(h, "GET", live+"api/notes", "", "Authorization", "Bearer "+token); w.Code != 200 {
		t.Fatalf("curl with a token: %d %s", w.Code, w.Body)
	}
	if got := rec.last(); got != "" {
		t.Fatalf("the backend saw the workspace token: %q", got)
	}
	if w := call(h, "GET", live+"api/notes", "", "Cookie", owner, "Authorization", "Bearer app-secret"); w.Code != 200 || rec.last() != "Bearer app-secret" {
		t.Fatalf("the app's own Authorization: %d, backend saw %q", w.Code, rec.last())
	}
	id2 := s.Auth.Tokens()[0].ID
	if err := s.Auth.RevokeToken(id2); err != nil {
		t.Fatal(err)
	}
	if w := call(h, "GET", live+"api/notes", "", "Authorization", "Bearer "+token); w.Code != 401 {
		t.Fatalf("curl with a revoked token: %d", w.Code)
	}

	// Signing that browser out ends the app sessions its pages were given.
	if err := s.Auth.EndSession(session); err != nil {
		t.Fatal(err)
	}
	if w := call(h, "GET", live+"api/notes", "", "Origin", "null", SessionHeader, appSession); w.Code != 403 {
		t.Fatalf("an app session after signing out: %d", w.Code)
	}
}
