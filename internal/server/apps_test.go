package server

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/LaneBirmingham/shartifacts/internal/runner"
	"github.com/LaneBirmingham/shartifacts/internal/store"
)

func appServer(t *testing.T, rt func(st *store.Store) runner.Runtime) (*Server, http.Handler) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: st, Web: testWeb(), ShareOrigin: "https://share.example.com"}
	if rt != nil {
		s.Runtime = rt(st)
	} else {
		local := &runner.Local{Dir: st.AppsDir(), GoBinary: filepath.Join(runtime.GOROOT(), "bin", "go")}
		t.Cleanup(local.Close)
		s.Runtime = local
	}
	return s, s.Handler()
}

// call sends a request as a LAN client would; headers come in pairs.
func call(h http.Handler, method, target, body string, headers ...string) *httptest.ResponseRecorder {
	if !strings.Contains(target, "://") {
		target = "http://127.0.0.1:8080" + target
	}
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.RemoteAddr = "192.168.1.9:12345"
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%d %s: %v", w.Code, w.Body.String(), err)
	}
	return out
}

var sessionInPage = regexp.MustCompile(`,T="([^"]*)"`)

func TestApps(t *testing.T) {
	_, h := appServer(t, nil)
	w := call(h, "POST", "/api/apps", `{"name":"Notes","template":"notes"}`)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	created := decodeBody(t, w)
	dep := created["deployment"].(map[string]any)
	if dep["status"] != "ready" || dep["backend"] != "go" || dep["frontend"] != true {
		t.Fatalf("first deploy: %v", dep)
	}
	id := created["app"].(map[string]any)["id"].(string)
	live := "/run/" + id + "/live/"

	// The page: a base for relative URLs, the runtime with a session, the bundle, the sandbox.
	page := call(h, "GET", live, "")
	body := page.Body.String()
	csp := page.Header().Get("Content-Security-Policy")
	if page.Code != 200 || !strings.Contains(body, `<base href="`+live+`">`) || !strings.Contains(body, `src="/vendor/react.js"`) || !regexp.MustCompile(`_build/main-\w+\.js`).MatchString(body) {
		t.Fatalf("page: %d %s", page.Code, body)
	}
	for _, want := range []string{"sandbox allow-scripts", "frame-ancestors 'self'", "connect-src http://127.0.0.1:8080" + live} {
		if !strings.Contains(csp, want) {
			t.Fatalf("CSP lacks %q: %s", want, csp)
		}
	}
	session := sessionInPage.FindStringSubmatch(body)[1]
	bundle := regexp.MustCompile(`_build/main-\w+\.js`).FindString(body)
	if w := call(h, "GET", live+bundle, ""); w.Code != 200 || w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("bundle: %d", w.Code)
	}
	if w := call(h, "GET", "/run/"+id+"/1/"+bundle, ""); !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
		t.Fatal("a numbered version's files should be immutable")
	}

	// Backend material is never served; client-side routes get the page.
	for _, p := range []string{"server/main.go", "go.mod", "shartifacts.json", "src/../server/main.go"} {
		// The mux redirects a path with .. to its clean form, which is refused in turn.
		if w := call(h, "GET", live+p, ""); w.Code != 404 && w.Code != 307 || strings.Contains(w.Body.String(), "package main") {
			t.Fatalf("%s served: %d", p, w.Code)
		}
	}
	if w := call(h, "GET", live+"notes/42", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "<base href=") {
		t.Fatalf("SPA route: %d", w.Code)
	}

	// Agents and curl (no Origin) reach the backend directly.
	if w := call(h, "POST", live+"api/notes", `{"text":"first"}`); w.Code != 201 {
		t.Fatalf("add note: %d %s", w.Code, w.Body)
	}
	// The sandboxed page's calls come from "null": only with its session.
	if w := call(h, "GET", live+"api/notes", "", "Origin", "null"); w.Code != 403 {
		t.Fatalf("null origin without session: %d", w.Code)
	}
	if w := call(h, "GET", live+"api/notes", "", "Origin", "null", SessionHeader, session); w.Code != 200 || !strings.Contains(w.Body.String(), "first") {
		t.Fatalf("null origin with session: %d %s", w.Code, w.Body)
	}
	if w := call(h, "GET", live+"api/notes?__st="+url.QueryEscape(session), "", "Origin", "null"); w.Code != 200 {
		t.Fatalf("session in the query (EventSource, WebSocket): %d", w.Code)
	}
	if w := call(h, "GET", "/run/"+id+"/1/api/notes", "", "Origin", "null", SessionHeader, session); w.Code != 403 {
		t.Fatalf("a live page's session reached another version: %d", w.Code)
	}
	if w := call(h, "POST", live+"api/notes", `{"text":"x"}`, "Origin", "https://evil.example", SessionHeader, session); w.Code != 403 {
		t.Fatalf("foreign origin: %d", w.Code)
	}
	if w := call(h, "OPTIONS", live+"api/notes", "", "Origin", "null", "Access-Control-Request-Method", "POST", "Access-Control-Request-Headers", "content-type,x-shartifacts-session"); w.Code != 204 || w.Header().Get("Access-Control-Allow-Headers") == "" {
		t.Fatalf("preflight: %d", w.Code)
	}

	// Identity comes from shartifacts, never from the client.
	person := decodeBody(t, call(h, "POST", "/api/people", `{"name":"Ada"}`))
	pid := person["id"].(string)
	if me := decodeBody(t, call(h, "GET", live+"api/me", "", "X-Shartifacts-Viewer", pid)); me["name"] != "Ada" || me["access"] != "lan" {
		t.Fatalf("viewer: %v", me)
	}
	if me := decodeBody(t, call(h, "GET", live+"api/me", "", "X-Shartifacts-User", `{"name":"mallory"}`)); me["name"] != "" {
		t.Fatalf("forged identity reached the backend: %v", me)
	}
	viewerPage := call(h, "GET", live+"?viewer="+pid, "").Body.String()
	viewerSession := sessionInPage.FindStringSubmatch(viewerPage)[1]
	if me := decodeBody(t, call(h, "GET", live+"api/me", "", "Origin", "null", SessionHeader, viewerSession)); me["name"] != "Ada" {
		t.Fatalf("viewer through the page session: %v", me)
	}

	// Version 2: a small backend change by edit; data carries over, version 1 keeps running.
	w = call(h, "POST", "/api/apps/"+id+"/deployments", `{"base_version":1,"merge":true,"note":"health says hello","edits":[{"file":"server/main.go","old_str":"\"ok\": true,","new_str":"\"ok\": true, \"greeting\": os.Getenv(\"GREETING\"),"}]}`)
	if w.Code != 201 {
		t.Fatalf("deploy v2: %d %s", w.Code, w.Body)
	}
	if d := decodeBody(t, w)["deployment"].(map[string]any); d["status"] != "ready" {
		t.Fatalf("v2: %v", d)
	}
	if w := call(h, "PUT", "/api/apps/"+id+"/env", `{"vars":{"GREETING":"hi there"}}`); w.Code != 200 || strings.Contains(w.Body.String(), "hi there") {
		t.Fatalf("env set (values never returned): %d %s", w.Code, w.Body)
	}
	health := decodeBody(t, call(h, "GET", live+"api/health", ""))
	if health["version"] != "2" || health["greeting"] != "hi there" {
		t.Fatalf("v2 health: %v", health)
	}
	if old := decodeBody(t, call(h, "GET", "/run/"+id+"/1/api/health", "")); old["version"] != "1" || old["greeting"] != nil {
		t.Fatalf("v1 health: %v", old)
	}
	if w := call(h, "GET", live+"api/notes", ""); !strings.Contains(w.Body.String(), "first") {
		t.Fatal("data did not carry over to version 2")
	}

	// A broken build never goes live.
	w = call(h, "POST", "/api/apps/"+id+"/deployments", `{"base_version":2,"merge":true,"edits":[{"file":"server/main.go","old_str":"package main","new_str":"package main\nfunc broken( {"}]}`)
	d3 := decodeBody(t, w)["deployment"].(map[string]any)
	if d3["status"] != "failed" || !strings.Contains(d3["error"].(string), "go build failed") {
		t.Fatalf("broken build: %v", d3)
	}
	if a := decodeBody(t, call(h, "GET", "/api/apps/"+id, "")); a["live"].(float64) != 2 {
		t.Fatalf("live after a failed build: %v", a["live"])
	}
	if w := call(h, "GET", "/run/"+id+"/3/", ""); w.Code != 404 {
		t.Fatalf("failed version served: %d", w.Code)
	}

	// Roll back.
	if w := call(h, "PUT", "/api/apps/"+id+"/live", `{"version":1}`); w.Code != 200 {
		t.Fatalf("rollback: %d %s", w.Code, w.Body)
	}
	if v := decodeBody(t, call(h, "GET", live+"api/health", ""))["version"]; v != "1" {
		t.Fatalf("after rollback: %v", v)
	}
	call(h, "PUT", "/api/apps/"+id+"/live", `{"version":2}`)

	// Logs show the backend's own output.
	logs := decodeBody(t, call(h, "GET", "/api/apps/"+id+"/logs?version=2", ""))
	if b, _ := json.Marshal(logs["backend"]); !strings.Contains(string(b), "notes listening") {
		t.Fatalf("logs: %s", b)
	}

	// Public links, on the share hostname only.
	share := func(method, path, body string, headers ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://share.example.com"+path, strings.NewReader(body))
		r.RemoteAddr = "172.18.0.1:4000"
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		for i := 0; i+1 < len(headers); i += 2 {
			r.Header.Set(headers[i], headers[i+1])
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	ro := decodeBody(t, call(h, "POST", "/api/apps/"+id+"/shares", `{"version":0,"read_only":true}`))
	full := decodeBody(t, call(h, "POST", "/api/apps/"+id+"/shares", `{"version":1}`))
	if !strings.HasPrefix(ro["url"].(string), "https://share.example.com/p/") {
		t.Fatalf("share url: %v", ro)
	}
	rop, fullp := "/p/"+ro["token"].(string)+"/", "/p/"+full["token"].(string)+"/"
	if w := share("GET", rop, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `<base href="`+rop+`">`) || !strings.Contains(w.Body.String(), `T=""`) {
		t.Fatalf("share page: %d", w.Code)
	}
	if w := share("GET", rop+"api/notes", "", "Origin", "null"); w.Code != 200 {
		t.Fatalf("share read: %d %s", w.Code, w.Body)
	}
	if me := decodeBody(t, share("GET", rop+"api/me", "", "Origin", "null", "X-Shartifacts-Viewer", pid)); me["name"] != "" || me["access"] != "public-read-only" {
		t.Fatalf("public visitor identity: %v", me)
	}
	if w := share("POST", rop+"api/notes", `{"text":"spam"}`, "Origin", "null"); w.Code != 403 {
		t.Fatalf("read-only share write: %d", w.Code)
	}
	// A read-only link refuses what could write past its GETs: WebSockets, bare OPTIONS.
	if w := share("GET", rop+"api/notes", "", "Origin", "null", "Upgrade", "websocket", "Connection", "Upgrade"); w.Code != 403 {
		t.Fatalf("read-only share upgrade: %d", w.Code)
	}
	if w := share("OPTIONS", rop+"api/notes", ""); w.Code != 403 {
		t.Fatalf("read-only share OPTIONS: %d", w.Code)
	}
	// Naming a trusted header in Connection must not strip it on the way to the backend.
	if me := decodeBody(t, share("GET", rop+"api/me", "", "Origin", "null", "Connection", "X-Shartifacts-Access")); me["access"] != "public-read-only" {
		t.Fatalf("Connection stripped a trusted header: %v", me)
	}
	// Encoded dot segments never reach the backend as real ones.
	if w := share("GET", fullp+"api/%2e%2e/%2e%2e/etc/passwd", "", "Origin", "null"); w.Code != 400 {
		t.Fatalf("encoded dot segments: %d %s", w.Code, w.Body)
	}
	if w := share("POST", fullp+"api/notes", `{"text":"from outside"}`, "Origin", "null"); w.Code != 201 {
		t.Fatalf("full share write: %d %s", w.Code, w.Body)
	}
	if v := decodeBody(t, share("GET", fullp+"api/health", ""))["version"]; v != "1" {
		t.Fatalf("pinned share serves version %v", v)
	}
	for _, p := range []string{"/api/apps", live, "/apps/" + id, "/api/apps/" + id + "/logs"} {
		if w := share("GET", p, ""); w.Code != 404 {
			t.Fatalf("share host served %s: %d", p, w.Code)
		}
	}
	call(h, "DELETE", "/api/app-shares/"+ro["token"].(string), "")
	if w := share("GET", rop, ""); w.Code != 404 {
		t.Fatalf("revoked share: %d", w.Code)
	}

	// Fix the failed version by editing it: merge starts from the latest version.
	w = call(h, "POST", "/api/apps/"+id+"/deployments", `{"base_version":3,"merge":true,"note":"fix","edits":[{"file":"server/main.go","old_str":"\nfunc broken( {","new_str":""}]}`)
	if d := decodeBody(t, w)["deployment"].(map[string]any); d["status"] != "ready" {
		t.Fatalf("fixing deploy: %v", d)
	}

	// An archive with one top-level folder, overlaid on the latest version.
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	f, _ := zw.Create("myapp/about.html")
	io.WriteString(f, "<!doctype html><html><head><title>About</title></head><body>about page</body></html>")
	zw.Close()
	r := httptest.NewRequest("POST", "http://127.0.0.1:8080/api/apps/"+id+"/deployments?merge=true&note=zip", &zbuf)
	r.RemoteAddr = "192.168.1.9:1"
	r.Header.Set("Content-Type", "application/zip")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if d := decodeBody(t, w)["deployment"].(map[string]any); d["status"] != "ready" {
		t.Fatalf("archive deploy: %v", d)
	}
	if w := call(h, "GET", live+"about.html", ""); !strings.Contains(w.Body.String(), "about page") {
		t.Fatalf("archive page: %s", w.Body)
	}
	if w := call(h, "GET", live+"api/notes", ""); w.Code != 200 {
		t.Fatal("merge dropped the backend")
	}

	if w := call(h, "DELETE", "/api/apps/"+id, ""); w.Code != 200 {
		t.Fatalf("delete: %d", w.Code)
	}
	if w := call(h, "GET", live, ""); w.Code != 404 {
		t.Fatalf("deleted app served: %d", w.Code)
	}
}

func TestAppFrontendBuildErrors(t *testing.T) {
	_, h := appServer(t, nil)
	create := func(files map[string]string) map[string]any {
		b, _ := json.Marshal(map[string]any{"name": "x", "files": files})
		return decodeBody(t, call(h, "POST", "/api/apps", string(b)))["deployment"].(map[string]any)
	}
	d := create(map[string]string{"index.html": `<script type="module" src="/src/main.tsx"></script>`, "src/main.tsx": `import { z } from "zod"; console.log(z)`})
	if d["status"] != "failed" || !strings.Contains(d["error"].(string), `src/main.tsx:1`) || !strings.Contains(d["error"].(string), "frontend.root") {
		t.Fatalf("unknown package: %v", d)
	}
	d = create(map[string]string{"index.html": "<h1>hi</h1>", "shartifacts.json": `{"backend":{"nework":true}}`})
	if d["status"] != "failed" || !strings.Contains(d["error"].(string), "nework") {
		t.Fatalf("manifest typo: %v", d)
	}
	d = create(map[string]string{"README.md": "nothing to serve"})
	if d["status"] != "failed" || !strings.Contains(d["error"].(string), "nothing to serve") {
		t.Fatalf("empty app: %v", d)
	}
	// Libraries resolve to the vendored bundles; @/ is src/.
	d = create(map[string]string{"index.html": `<html><head></head><body><div id="root"></div><script type="module" src="./src/main.tsx"></script></body></html>`,
		"src/main.tsx":  `import { createRoot } from "react-dom/client"; import { LineChart } from "recharts"; import Title from "@/Title"; createRoot(document.getElementById("root")!).render(<div className="flex p-4"><Title/>{String(LineChart)}</div>)`,
		"src/Title.tsx": `export default function Title() { return <h1>Chart</h1> }`})
	if d["status"] != "ready" || d["backend"] != nil {
		t.Fatalf("frontend-only app: %v", d)
	}
}

// The compose stack runs backends in a runner sidecar; the same flow works through it.
func TestAppsThroughRunnerSidecar(t *testing.T) {
	t.Setenv("SHARTIFACTS_RUNNER_TOKEN", "")
	var sidecar *httptest.Server
	_, h := appServer(t, func(st *store.Store) runner.Runtime {
		local := &runner.Local{Dir: st.AppsDir(), GoBinary: filepath.Join(runtime.GOROOT(), "bin", "go"), Remote: true}
		t.Cleanup(local.Close)
		sidecar = httptest.NewServer(runner.Handler(local))
		t.Cleanup(sidecar.Close)
		token, err := runner.EnsureToken(st.AppsDir())
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(sidecar.URL)
		return &runner.Remote{URL: u, Token: token}
	})
	if res, _ := http.Get(sidecar.URL + "/info"); res.StatusCode != 401 {
		t.Fatalf("runner API without the token: %d", res.StatusCode)
	}
	w := call(h, "POST", "/api/apps", `{"name":"Notes","template":"notes"}`)
	created := decodeBody(t, w)
	if d := created["deployment"].(map[string]any); d["status"] != "ready" {
		t.Fatalf("deploy through the runner: %v", d)
	}
	id := created["app"].(map[string]any)["id"].(string)
	if w := call(h, "POST", "/run/"+id+"/live/api/notes", `{"text":"via sidecar"}`); w.Code != 201 {
		t.Fatalf("API through the runner: %d %s", w.Code, w.Body)
	}
	if info := decodeBody(t, call(h, "GET", "/api/apps", ""))["runtime"].(map[string]any); info["remote"] != true || info["go"] == "" {
		t.Fatalf("runtime info: %v", info)
	}
	logs := decodeBody(t, call(h, "GET", "/api/apps/"+id+"/logs", ""))
	if b, _ := json.Marshal(logs["backend"]); !strings.Contains(string(b), "running") {
		t.Fatalf("logs through the runner: %s", b)
	}
	if err := (&runner.Remote{URL: mustURL(sidecar.URL), Token: "wrong"}).Start(context.Background(), id, 1); err == nil {
		t.Fatal("runner accepted a wrong token")
	}
}

func mustURL(s string) *url.URL {
	u, _ := url.Parse(s)
	return u
}

const streamingBackend = `package main

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

func main() {
	http.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 1; i <= 2; i++ {
			fmt.Fprintf(w, "data: %d\n\n", i)
			w.(http.Flusher).Flush()
			time.Sleep(400 * time.Millisecond)
		}
	})
	// A raw protocol upgrade, as a WebSocket handshake is: echo one line back.
	http.HandleFunc("/api/echo", func(w http.ResponseWriter, r *http.Request) {
		conn, rw, _ := w.(http.Hijacker).Hijack()
		defer conn.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
		rw.Flush()
		line, _ := bufio.NewReader(conn).ReadString('\n')
		conn.Write([]byte("echo " + line))
	})
	http.ListenAndServe(net.JoinHostPort(os.Getenv("HOST"), os.Getenv("PORT")), nil)
}
`

// Event streams arrive as they are written, and upgraded connections (WebSockets) pass
// through, directly and through the runner sidecar.
func TestAppStreaming(t *testing.T) {
	for _, viaSidecar := range []bool{false, true} {
		var rt func(st *store.Store) runner.Runtime
		if viaSidecar {
			rt = func(st *store.Store) runner.Runtime {
				local := &runner.Local{Dir: st.AppsDir(), GoBinary: filepath.Join(runtime.GOROOT(), "bin", "go"), Remote: true}
				t.Cleanup(local.Close)
				sidecar := httptest.NewServer(runner.Handler(local))
				t.Cleanup(sidecar.Close)
				token, _ := runner.EnsureToken(st.AppsDir())
				return &runner.Remote{URL: mustURL(sidecar.URL), Token: token}
			}
		}
		_, h := appServer(t, rt)
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		b, _ := json.Marshal(map[string]any{"name": "Stream", "files": map[string]string{"server/main.go": streamingBackend}})
		created := decodeBody(t, call(h, "POST", "/api/apps", string(b)))
		if d := created["deployment"].(map[string]any); d["status"] != "ready" {
			t.Fatalf("deploy: %v", d)
		}
		live := srv.URL + "/run/" + created["app"].(map[string]any)["id"].(string) + "/live/"

		res, err := http.Get(live + "api/events")
		if err != nil {
			t.Fatal(err)
		}
		first := make([]byte, 64)
		start := time.Now()
		n, _ := res.Body.Read(first)
		if !strings.Contains(string(first[:n]), "data: 1") || time.Since(start) > 300*time.Millisecond {
			t.Fatalf("first event %q after %s: the stream was buffered (sidecar %v)", first[:n], time.Since(start), viaSidecar)
		}
		rest, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if !strings.Contains(string(rest), "data: 2") {
			t.Fatalf("second event: %q", rest)
		}

		conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(live + "api/echo")
		fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n", u.Path, u.Host)
		br := bufio.NewReader(conn)
		status, _ := br.ReadString('\n')
		if !strings.Contains(status, "101") {
			t.Fatalf("upgrade (sidecar %v): %q", viaSidecar, status)
		}
		for line, _ := br.ReadString('\n'); line != "\r\n" && line != ""; line, _ = br.ReadString('\n') {
		}
		fmt.Fprint(conn, "hello\n")
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if got, _ := br.ReadString('\n'); got != "echo hello\n" {
			t.Fatalf("upgraded connection (sidecar %v): %q", viaSidecar, got)
		}
		conn.Close()
	}
}

const cookieBackend = `package main

import (
	"net"
	"net/http"
	"os"
)

func main() {
	http.HandleFunc("/api/cookie", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "__Host-sid=abc; Path=/; Secure; HttpOnly; SameSite=Lax")
		w.Header().Add("Set-Cookie", "wide=1; Path=/; domain=share.example.com")
		w.Write([]byte("cookie:" + r.Header.Get("Cookie")))
	})
	http.ListenAndServe(net.JoinHostPort(os.Getenv("HOST"), os.Getenv("PORT")), nil)
}
`

// With AppOrigin, each app's public links get a hostname of their own: pages there keep
// their origin, the backend's cookies pass (kept to that hostname), and nothing but the
// app's own pages and links to them reaches it. Everywhere else cookies pass neither way.
func TestAppOrigins(t *testing.T) {
	for _, viaSidecar := range []bool{false, true} {
		var rt func(st *store.Store) runner.Runtime
		if viaSidecar {
			rt = func(st *store.Store) runner.Runtime {
				local := &runner.Local{Dir: st.AppsDir(), GoBinary: filepath.Join(runtime.GOROOT(), "bin", "go"), Remote: true}
				t.Cleanup(local.Close)
				sidecar := httptest.NewServer(runner.Handler(local))
				t.Cleanup(sidecar.Close)
				token, _ := runner.EnsureToken(st.AppsDir())
				return &runner.Remote{URL: mustURL(sidecar.URL), Token: token}
			}
		}
		s, h := appServer(t, rt)
		s.AppOrigin = "https://*.share.example.com"
		create := func(files map[string]string) string {
			b, _ := json.Marshal(map[string]any{"name": "Cookies", "files": files})
			created := decodeBody(t, call(h, "POST", "/api/apps", string(b)))
			if d := created["deployment"].(map[string]any); d["status"] != "ready" {
				t.Fatalf("deploy: %v", d)
			}
			return created["app"].(map[string]any)["id"].(string)
		}
		id := create(map[string]string{"index.html": "<h1>cookies</h1>", "server/main.go": cookieBackend})
		other := create(map[string]string{"index.html": "<h1>other</h1>"})
		share := func(app, body string) (string, string) {
			sh := decodeBody(t, call(h, "POST", "/api/apps/"+app+"/shares", body))
			return sh["url"].(string), sh["token"].(string)
		}
		link, token := share(id, `{"version":0}`)
		roLink, _ := share(id, `{"version":0,"read_only":true}`)
		otherLink, otherToken := share(other, `{"version":0}`)
		u, ro, ou := mustURL(link), mustURL(roLink), mustURL(otherLink)
		if !regexp.MustCompile(`^[a-z2-7]{16}\.share\.example\.com$`).MatchString(u.Host) || u.Scheme != "https" || u.Path != "/p/"+token+"/" {
			t.Fatalf("app link: %s", link)
		}
		if ro.Host != u.Host || ou.Host == u.Host {
			t.Fatalf("one hostname per app: %s %s %s", link, roLink, otherLink)
		}
		if listed := decodeBody(t, call(h, "GET", "/api/apps/"+id, "")); !strings.Contains(fmt.Sprint(listed["shares"]), link) {
			t.Fatalf("app view lists %v", listed["shares"])
		}

		// The page keeps its origin; someone following a link from anywhere gets it.
		nav := []string{"Sec-Fetch-Site", "cross-site", "Sec-Fetch-Mode", "navigate", "Sec-Fetch-Dest", "document"}
		page := call(h, "GET", link, "", nav...)
		if page.Code != 200 || !strings.Contains(page.Body.String(), `<base href="/p/`+token+`/">`) || !strings.Contains(page.Header().Get("Content-Security-Policy"), "allow-same-origin") {
			t.Fatalf("page on its own origin: %d %s %s", page.Code, page.Header().Get("Content-Security-Policy"), page.Body)
		}

		// Its own pages' calls carry cookies both ways; Domain attributes are dropped.
		w := call(h, "GET", link+"api/cookie", "", "Sec-Fetch-Site", "same-origin", "Cookie", "__Host-sid=abc")
		if w.Code != 200 || w.Body.String() != "cookie:__Host-sid=abc" {
			t.Fatalf("cookie to the backend (sidecar %v): %d %s", viaSidecar, w.Code, w.Body)
		}
		if got := w.Header().Values("Set-Cookie"); len(got) != 2 || got[0] != "__Host-sid=abc; Path=/; Secure; HttpOnly; SameSite=Lax" || got[1] != "wide=1; Path=/" {
			t.Fatalf("cookies from the backend (sidecar %v): %q", viaSidecar, got)
		}
		if w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("an app's own origin answered CORS")
		}
		if w := call(h, "POST", link+"api/cookie", "{}", "Sec-Fetch-Site", "same-origin", "Origin", "https://"+u.Host); w.Code != 200 {
			t.Fatalf("own page's POST: %d %s", w.Code, w.Body)
		}
		if w := call(h, "GET", link+"api/cookie", ""); w.Code != 200 {
			t.Fatalf("curl: %d", w.Code)
		}

		// Nothing else calls it: not other sites, not other apps (same-site to a browser).
		for _, hs := range [][]string{
			{"Sec-Fetch-Site", "same-site", "Sec-Fetch-Mode", "cors"},
			{"Sec-Fetch-Site", "cross-site", "Sec-Fetch-Mode", "no-cors"},
			{"Origin", "https://evil.example"},
			{"Origin", "null"},
			{"Origin", "https://" + ou.Host},
		} {
			if w := call(h, "POST", link+"api/cookie", "{}", hs...); w.Code != 403 {
				t.Fatalf("%v reached the backend: %d", hs, w.Code)
			}
		}
		if w := call(h, "POST", link+"api/cookie", "{}", nav...); w.Code != 403 {
			t.Fatalf("cross-site form post: %d", w.Code)
		}

		// The hostname serves its app's links and nothing else.
		if w := call(h, "GET", "https://"+u.Host+"/p/"+otherToken+"/", ""); w.Code != 404 || w.Header().Get("Clear-Site-Data") != "" {
			t.Fatalf("another app's link on this hostname: %d %v", w.Code, w.Header())
		}
		if w := call(h, "GET", otherLink, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "other") {
			t.Fatalf("other app on its hostname: %d", w.Code)
		}
		for _, p := range []string{"/", "/api/apps", "/run/" + id + "/live/", "/s/" + token, "/mcp"} {
			if w := call(h, "GET", "https://"+u.Host+p, ""); w.Code != 404 {
				t.Fatalf("app hostname served %s: %d", p, w.Code)
			}
		}
		if w := call(h, "GET", "https://www.share.example.com/p/"+token+"/", ""); w.Code != 403 {
			t.Fatalf("a hostname that is no app's label: %d", w.Code)
		}

		// The share hostname sends pages to the app's own origin, and still answers the
		// API for clients there, without cookies.
		if w := call(h, "GET", "https://share.example.com/p/"+token+"/notes?x=1", ""); w.Code != 302 || w.Header().Get("Location") != link+"notes?x=1" {
			t.Fatalf("share hostname page: %d %v", w.Code, w.Header())
		}
		w = call(h, "GET", "https://share.example.com/p/"+token+"/api/cookie", "", "Origin", "null", "Cookie", "a=b")
		if w.Code != 200 || w.Body.String() != "cookie:" || len(w.Header().Values("Set-Cookie")) != 0 || w.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("share hostname API (sidecar %v): %d %s %v", viaSidecar, w.Code, w.Body, w.Header())
		}
		// The network's URLs are the workspace's origin: no cookies either way.
		w = call(h, "GET", "/run/"+id+"/live/api/cookie", "", "Cookie", "workspace=1")
		if w.Body.String() != "cookie:" || len(w.Header().Values("Set-Cookie")) != 0 {
			t.Fatalf("LAN API (sidecar %v): %s %v", viaSidecar, w.Body, w.Header())
		}

		// Gone links answer 404; a gone app also clears what its pages kept.
		call(h, "DELETE", "/api/app-shares/"+token, "")
		if w := call(h, "GET", link, ""); w.Code != 404 || w.Header().Get("Clear-Site-Data") != "" {
			t.Fatalf("revoked link: %d %v", w.Code, w.Header())
		}
		call(h, "DELETE", "/api/apps/"+id, "")
		if w := call(h, "GET", roLink, ""); w.Code != 404 || w.Header().Get("Clear-Site-Data") != `"storage"` {
			t.Fatalf("deleted app's hostname: %d %v", w.Code, w.Header())
		}
	}
}

// An app may be given the label its links use (budget.share.example.com): the hostnames it
// had before send people there and stay its own, and no other app may take any of them.
func TestAppLabels(t *testing.T) {
	s, h := appServer(t, nil)
	s.AppOrigin = "https://*.share.example.com"
	nav := []string{"Sec-Fetch-Site", "cross-site", "Sec-Fetch-Mode", "navigate", "Sec-Fetch-Dest", "document"}
	create := func(body string) map[string]any {
		t.Helper()
		created := decodeBody(t, call(h, "POST", "/api/apps", body))
		if created["app"] == nil {
			t.Fatalf("create: %v", created)
		}
		return created["app"].(map[string]any)
	}
	share := func(id string) string {
		return decodeBody(t, call(h, "POST", "/api/apps/"+id+"/shares", `{"version":0}`))["token"].(string)
	}
	label := func(id, l string) int {
		return call(h, "PATCH", "/api/apps/"+id, `{"label":"`+l+`"}`).Code
	}
	page := func(link string) *httptest.ResponseRecorder { return call(h, "GET", link, "", nav...) }

	app := create(`{"name":"Budget","label":"budget","files":{"index.html":"<h1>budget</h1>"}}`)
	id := app["id"].(string)
	if app["label"] != "budget" || app["link_origin"] != "https://budget.share.example.com" {
		t.Fatalf("created with a label: %v", app)
	}
	token := share(id)
	link := "https://budget.share.example.com/p/" + token + "/"
	if listed := decodeBody(t, call(h, "GET", "/api/apps/"+id, "")); !strings.Contains(fmt.Sprint(listed["shares"]), link) {
		t.Fatalf("link on the label: %v", listed["shares"])
	}
	if w := page(link); w.Code != 200 || !strings.Contains(w.Body.String(), "budget") {
		t.Fatalf("page on the label: %d", w.Code)
	}
	generated := "https://" + s.generatedLabel(id) + ".share.example.com/p/" + token + "/"
	if w := page(generated + "x?y=1"); w.Code != 302 || w.Header().Get("Location") != link+"x?y=1" {
		t.Fatalf("generated hostname: %d %v", w.Code, w.Header())
	}

	other := create(`{"name":"Other","files":{"index.html":"<h1>other</h1>"}}`)
	otherID := other["id"].(string)
	otherGenerated := strings.TrimSuffix(strings.TrimPrefix(other["link_origin"].(string), "https://"), ".share.example.com")
	for _, bad := range []string{"Budget", "-x", "x-", "a--b", "a.b", strings.Repeat("a", 41), otherGenerated, "budget"} {
		if code := label(otherID, bad); code != 400 {
			t.Fatalf("label %q accepted: %d", bad, code)
		}
	}
	if w := page("https://budget.share.example.com/p/" + share(otherID) + "/"); w.Code != 404 {
		t.Fatalf("another app's link on a label: %d", w.Code)
	}
	if w := page("https://nobody.share.example.com/p/" + token + "/"); w.Code != 403 {
		t.Fatalf("a label no app has: %d", w.Code)
	}

	// Renamed, the old label redirects and stays the app's; the API still answers there
	// for pages already open on it.
	if code := label(id, "money"); code != 200 {
		t.Fatalf("rename: %d", code)
	}
	money := "https://money.share.example.com/p/" + token + "/"
	for _, old := range []string{link, generated} {
		if w := page(old); w.Code != 302 || w.Header().Get("Location") != money {
			t.Fatalf("%s after rename: %d %v", old, w.Code, w.Header())
		}
	}
	if w := call(h, "GET", link+"api/x", "", "Sec-Fetch-Site", "same-origin"); w.Code == 302 || w.Code == 403 {
		t.Fatalf("API on the former label: %d", w.Code)
	}
	if w := page(money); w.Code != 200 {
		t.Fatalf("renamed page: %d", w.Code)
	}
	if code := label(otherID, "budget"); code != 400 {
		t.Fatalf("another app took a former label: %d", code)
	}
	// Back to budget, then to the generated label.
	if label(id, "budget") != 200 || page(link).Code != 200 || page(money).Header().Get("Location") != link {
		t.Fatal("back to a former label")
	}
	if label(id, "") != 200 || page(generated).Code != 200 || page(link).Header().Get("Location") != generated {
		t.Fatal("back to the generated label")
	}

	// Deleting the app frees its labels.
	call(h, "DELETE", "/api/apps/"+id, "")
	if w := page(link); w.Code != 403 {
		t.Fatalf("deleted app's label: %d", w.Code)
	}
	if code := label(otherID, "budget"); code != 200 {
		t.Fatalf("freed label: %d", code)
	}

	// Labels naming the server's own hostnames are refused, and every label is when apps
	// have no hostnames of their own; clearing one still works.
	s.AllowedHosts = []string{"ui.share.example.com"}
	if code := label(otherID, "ui"); code != 400 {
		t.Fatalf("label on an allowed host: %d", code)
	}
	s.AppOrigin = ""
	if code := label(otherID, "money"); code != 400 {
		t.Fatalf("label with app origins off: %d", code)
	}
	if code := label(otherID, ""); code != 200 {
		t.Fatalf("clearing with app origins off: %d", code)
	}
}

// The three app tools cover what the eleven did: every manage_app op, and their refusals.
func TestAppTools(t *testing.T) {
	_, h := appServer(t, nil)
	seq := 0
	tool := func(name string, args map[string]any) (map[string]any, bool) {
		t.Helper()
		seq++
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": seq, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
		res := mcpRequest(t, h, "http://127.0.0.1:8080", string(b))
		if res["isError"] == true {
			return map[string]any{"error": res["content"].([]any)[0].(map[string]any)["text"]}, false
		}
		if sc, ok := res["structuredContent"].(map[string]any); ok {
			return sc, true
		}
		return map[string]any{"text": res["content"].([]any)[0].(map[string]any)["text"]}, true
	}
	if g, ok := tool("get_app", map[string]any{"guide": true}); !ok || !strings.Contains(g["text"].(string), "Backend contract") {
		t.Fatalf("guide: %v", g)
	}
	if res, ok := tool("deploy_app", map[string]any{"name": "x", "merge": true, "files": map[string]string{"index.html": "x"}}); ok {
		t.Fatalf("merge without an app accepted: %v", res)
	}
	created, ok := tool("deploy_app", map[string]any{"name": "Two", "files": map[string]string{"index.html": "<h1>one</h1>"}})
	if !ok || created["status"] != "ready" {
		t.Fatalf("create: %v", created)
	}
	id := created["id"].(string)
	if res, ok := tool("deploy_app", map[string]any{"id": id, "template": "notes", "base_version": 1}); ok {
		t.Fatalf("template on an existing app accepted: %v", res)
	}
	v2, _ := tool("deploy_app", map[string]any{"id": id, "base_version": 1, "merge": true, "edits": []map[string]string{{"file": "index.html", "old_str": "one", "new_str": "two"}}})
	if v2["status"] != "ready" || v2["live"].(float64) != 2 {
		t.Fatalf("deploy: %v", v2)
	}
	if got, _ := tool("manage_app", map[string]any{"id": id, "op": "promote", "version": 1}); got["live"].(float64) != 1 {
		t.Fatalf("promote: %v", got)
	}
	upd, _ := tool("manage_app", map[string]any{"id": id, "op": "update", "name": "Renamed", "pinned": true, "env": map[string]any{"TOKEN": "s3cret"}})
	if upd["name"] != "Renamed" || upd["pinned"] != true || strings.Contains(fmt.Sprint(upd), "s3cret") {
		t.Fatalf("update: %v", upd)
	}
	if res, ok := tool("manage_app", map[string]any{"id": id, "op": "update", "label": "two"}); ok || !strings.Contains(res["error"].(string), "-app-origin") {
		t.Fatalf("label with app origins off: %v", res)
	}
	if res, ok := tool("manage_app", map[string]any{"id": id, "op": "call", "path": "/api/x"}); ok {
		t.Fatalf("call on a frontend-only app: %v", res)
	}
	if res, ok := tool("manage_app", map[string]any{"id": id, "op": "restart"}); !ok || res["restarted"] != true {
		t.Fatalf("restart: %v", res)
	}
	sh, _ := tool("manage_app", map[string]any{"id": id, "op": "share", "version": 0})
	if res, ok := tool("manage_app", map[string]any{"id": id, "op": "unshare", "token": sh["token"]}); !ok || res["revoked"] != true {
		t.Fatalf("unshare: %v", res)
	}
	if res, ok := tool("manage_app", map[string]any{"id": id, "op": "explode"}); ok || !strings.Contains(res["error"].(string), "op must be") {
		t.Fatalf("unknown op: %v", res)
	}
	got, _ := tool("get_app", map[string]any{"id": id, "list_files": true, "files": []string{"index.html"}, "version": 2, "logs": true})
	if got["contents"].(map[string]any)["index.html"] != "<h1>two</h1>" || got["files"] == nil || got["logs"] == nil {
		t.Fatalf("get_app with options: %v", got)
	}
	ask, _ := tool("manage_app", map[string]any{"id": id, "op": "delete"})
	if res, ok := tool("manage_app", map[string]any{"id": id, "op": "delete", "confirm": "wrong"}); ok {
		t.Fatalf("delete with a wrong token: %v", res)
	}
	if res, ok := tool("manage_app", map[string]any{"id": id, "op": "delete", "confirm": ask["confirm"]}); !ok || res["deleted"] != true {
		t.Fatalf("delete: %v", res)
	}
	if list, _ := tool("get_app", nil); len(list["apps"].([]any)) != 0 {
		t.Fatalf("list after delete: %v", list)
	}
}

func TestServerOnly(t *testing.T) {
	custom := appSite{Backend: "./backend", Binary: "bin/app"}
	for p, want := range map[string]bool{
		"server/main.go": true, "go.mod": true, "vendor/x/y.txt": true, "shartifacts.json": true,
		"backend/config.yaml": true, "backend": true, "bin/app": true, "lib/util.go": true,
		"index.html": false, "src/main.tsx": false, "backends/readme.txt": false, "public/backend.png": false,
	} {
		if got := serverOnly(p, custom); got != want {
			t.Errorf("%s: %v, want %v", p, got, want)
		}
	}
	if serverOnly("index.html", appSite{Backend: "."}) {
		t.Error("a root package hides the frontend")
	}
}
