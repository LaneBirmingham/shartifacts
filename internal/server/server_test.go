package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/LaneBirmingham/shartifacts/internal/store"
)

func testServer(t *testing.T) http.Handler {
	t.Helper()
	st, e := store.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	s := &Server{Store: st, Web: fstest.MapFS{"index.html": {Data: []byte("hello")}, "hub.html": {Data: []byte("apps")}}, AllowedHosts: []string{"myserver.tailnet.ts.net", "shartifacts.example.com"}, PublicOrigin: "https://shartifacts.example.com"}
	return s.Handler()
}
func request(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://127.0.0.1:8080"+path, strings.NewReader(body))
	r.RemoteAddr = "192.168.1.9:12345"
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestNetworkGuard(t *testing.T) {
	h := testServer(t)
	for _, tc := range []struct {
		name, method, peer, host, origin, site, mode, dest, path, xfp string
		want                                                          int
	}{
		{"LAN", "", "192.168.1.9:2", "192.168.1.2:8080", "", "", "", "", "/healthz", "", 200},
		{"tailnet", "", "100.101.102.103:2", "myserver.tailnet.ts.net:8080", "", "", "", "", "/healthz", "", 200},
		{"v6 LAN", "", "[fd12::1]:2", "[fd12::2]:8080", "", "", "", "", "/healthz", "", 200},
		{"mapped v4", "", "[::ffff:192.168.1.9]:2", "127.0.0.1:8080", "", "", "", "", "/healthz", "", 200},
		{"public peer", "", "8.8.8.8:2", "192.168.1.2:8080", "", "", "", "", "/healthz", "", 403},
		{"rebinding host", "", "192.168.1.9:2", "attacker.example", "", "", "", "", "/healthz", "", 403},
		{"external origin", "", "192.168.1.9:2", "192.168.1.2:8080", "https://evil.example", "", "", "", "/healthz", "", 403},
		{"opaque origin", "", "192.168.1.9:2", "192.168.1.2:8080", "null", "", "", "", "/api/artifacts", "", 403},
		{"cross site", "", "192.168.1.9:2", "192.168.1.2:8080", "", "cross-site", "", "", "/api/artifacts", "", 403},
		{"same origin", "", "192.168.1.9:2", "192.168.1.2:8080", "http://192.168.1.2:8080", "", "", "", "/healthz", "", 200},
		// Tapping a link into the app is a top-level navigation and must work, or the
		// page is unreachable from any chat app or bookmark.
		{"cross-site link navigation", "", "100.101.102.103:2", "myserver.tailnet.ts.net:10001", "", "cross-site", "navigate", "document", "/", "", 200},
		{"cross-site artifact navigation", "", "100.101.102.103:2", "myserver.tailnet.ts.net:10001", "", "cross-site", "navigate", "document", "/a/abc", "", 200},
		// The guard still blocks what it exists for: cross-site subresource and API traffic.
		{"cross-site fetch", "", "192.168.1.9:2", "192.168.1.2:8080", "", "cross-site", "cors", "empty", "/api/artifacts", "", 403},
		{"cross-site script", "", "192.168.1.9:2", "192.168.1.2:8080", "", "cross-site", "no-cors", "script", "/app.js", "", 403},
		// A cross-site form POST also navigates; it must not reach the API.
		{"cross-site form post", "POST", "192.168.1.9:2", "192.168.1.2:8080", "", "cross-site", "navigate", "document", "/api/artifacts", "", 403},
		{"cross-site mcp post", "POST", "192.168.1.9:2", "192.168.1.2:8080", "", "cross-site", "cors", "empty", "/mcp", "", 403},
		// Behind a TLS-terminating proxy (nginx, Caddy, tailscale serve). It forwards plain
		// HTTP, so r.TLS is nil inside the app while the browser's Origin is https://: the
		// declared -public-origin supplies the scheme. Peer 172.18.0.1 is what a published
		// Docker port really shows (nginx's loopback connection arrives via docker-proxy).
		{"proxied https page", "", "172.18.0.1:2", "shartifacts.example.com", "", "", "", "", "/", "", 200},
		{"proxied https origin", "", "172.18.0.1:2", "shartifacts.example.com", "https://shartifacts.example.com", "", "", "", "/api/artifacts", "", 200},
		{"proxied https form", "POST", "172.18.0.1:2", "shartifacts.example.com", "https://shartifacts.example.com", "same-origin", "cors", "empty", "/api/artifacts", "", -1},
		{"proxied mcp post", "POST", "172.18.0.1:2", "shartifacts.example.com", "https://shartifacts.example.com", "same-origin", "cors", "empty", "/mcp", "", -1},
		// Forwarded headers stay ignored: the declared origin decides, so a proxy that
		// passes http (or an attacker who sets the header by hand) changes nothing.
		{"proxied with xfp http", "", "172.18.0.1:2", "shartifacts.example.com", "https://shartifacts.example.com", "", "", "", "/api/artifacts", "http", 200},
		{"xfp cannot fake https", "", "192.168.1.9:2", "myserver.tailnet.ts.net", "https://myserver.tailnet.ts.net", "", "", "", "/api/artifacts", "https", 403},
		// The guard still refuses cross-site API traffic through the proxy.
		{"proxy cross-site api", "", "172.18.0.1:2", "shartifacts.example.com", "", "cross-site", "cors", "empty", "/api/artifacts", "https", 403},
		{"proxy cross-site api with origin", "POST", "172.18.0.1:2", "shartifacts.example.com", "https://shartifacts.example.com", "cross-site", "cors", "empty", "/api/artifacts", "https", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			method := tc.method
			if method == "" {
				method = "GET"
			}
			r := httptest.NewRequest(method, "http://"+tc.host+tc.path, nil)
			r.RemoteAddr = tc.peer
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("Sec-Fetch-Site", tc.site)
			if tc.mode != "" {
				r.Header.Set("Sec-Fetch-Mode", tc.mode)
			}
			if tc.dest != "" {
				r.Header.Set("Sec-Fetch-Dest", tc.dest)
			}
			if tc.xfp != "" {
				r.Header.Set("X-Forwarded-Proto", tc.xfp)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if tc.want == -1 {
				// want -1: this case exists to prove the guard let a proxied https
				// request THROUGH; the endpoint's own status is not under test.
				if w.Code == 403 {
					t.Fatalf("guard rejected a proxied request: %s", w.Body.String())
				}
				return
			}
			if w.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}
func TestArtifactAPI(t *testing.T) {
	h := testServer(t)
	raw := `{"title":"Hello","kind":"html","content":"<h1>v1</h1>"}`
	w := request(h, "POST", "/api/artifacts", raw)
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	var a store.Artifact
	json.Unmarshal(w.Body.Bytes(), &a)
	update := `{"title":"Hello","kind":"html","content":"<h1>v2</h1>","base_version":1}`
	if w = request(h, "PUT", "/api/artifacts/"+a.ID, update); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w = request(h, "PUT", "/api/artifacts/"+a.ID, update); w.Code != 409 {
		t.Fatalf("conflict status: %d", w.Code)
	}
	w = request(h, "GET", "/api/artifacts/"+a.ID+"/download?version=1", "")
	if w.Code != 200 || w.Body.String() != "<h1>v1</h1>" || !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
		t.Fatal("download does not preserve original source")
	}
	w = request(h, "GET", "/preview/"+a.ID+"?version=1", "")
	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "sandbox allow-scripts") || !strings.Contains(csp, "connect-src http://127.0.0.1:8080/_blob/;") || !strings.Contains(w.Body.String(), "<h1>v1</h1>") {
		t.Fatal("preview sandbox or revision missing")
	}
	for _, path := range []string{"/preview/" + a.ID + "?version=0", "/preview/" + a.ID + "?version=3", "/api/artifacts/nope"} {
		if w = request(h, "GET", path, ""); w.Code != 404 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	if w = request(h, "DELETE", "/api/artifacts/"+a.ID+"?base_version=1", ""); w.Code != 409 {
		t.Fatal("stale delete accepted")
	}
	if w = request(h, "DELETE", "/api/artifacts/"+a.ID+"?base_version=2", ""); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
}
func TestPreviewDraft(t *testing.T) {
	h := testServer(t)
	w := request(h, "POST", "/api/previews", `{"title":"Draft","kind":"html","content":"<h1>unsaved</h1>"}`)
	var d map[string]string
	json.Unmarshal(w.Body.Bytes(), &d)
	w = request(h, "GET", d["url"], "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "unsaved") {
		t.Fatal("draft not rendered")
	}
	if w = request(h, "GET", d["url"], ""); w.Code != 404 {
		t.Fatal("preview token reused")
	}
	if w = request(h, "GET", "/api/artifacts", ""); strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatal("draft persisted as an artifact")
	}
}
func TestRejectBadJSON(t *testing.T) {
	h := testServer(t)
	for _, body := range []string{`{`, `{} {}`, `{"title":"x","kind":"html","unknown":1}`, `{"title":"x","kind":"unknown"}`} {
		if w := request(h, "POST", "/api/artifacts", body); w.Code != 400 {
			t.Fatalf("bad body accepted: %s", body)
		}
	}
}
func TestReactCompiler(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		valid        bool
	}{
		{"TSX hooks", `import React, {useState} from 'react'; export default function App(){const [n,set]=useState<number>(0);return <button onClick={()=>set(n+1)}>{n}</button>}`, true},
		{"implicit React", `export default function App(){return <h1>Hi</h1>}`, true},
		{"no filesystem imports", `import x from '/etc/passwd';export default x`, false},
		{"no remote imports", `import x from 'https://evil.example/a.js';export default x`, false},
		{"no unknown packages", `import x from 'left-pad';export default x`, false},
		{"no lodash path escapes", `import x from 'lodash/../../etc';export default x`, false},
		{"syntax error", `export default function App( {`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, _, _, e := compileReact(tc.source, nil)
			if (e == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, e)
			}
			if tc.valid && !strings.Contains(out, "ArtifactModule") {
				t.Fatal("missing module export")
			}
		})
	}
}
func TestMCPInitializeAndTools(t *testing.T) {
	h := testServer(t)
	call := func(body string) map[string]any {
		r := httptest.NewRequest("POST", "http://127.0.0.1:8080/mcp", bytes.NewBufferString(body))
		r.RemoteAddr = "127.0.0.1:22"
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("MCP %d: %s", w.Code, w.Body.String())
		}
		b, _ := io.ReadAll(w.Result().Body)
		var result map[string]any
		if e := json.Unmarshal(b, &result); e != nil {
			t.Fatalf("invalid MCP response: %s", b)
		}
		return result
	}
	out := call(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	if out["error"] != nil {
		t.Fatal(out)
	}
	instructions := out["result"].(map[string]any)["instructions"].(string)
	for _, part := range []string{"begin_media_upload", "upload_media_chunk", "256 MiB", "<video", "/api/media", "base_version"} {
		if !strings.Contains(instructions, part) {
			t.Fatalf("agent guidance missing %s", part)
		}
	}
	out = call(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	b, _ := json.Marshal(out)
	for _, name := range []string{"list_artifacts", "get_artifact", "create_artifact", "update_artifact", "list_media", "begin_media_upload", "upload_media_chunk"} {
		if !bytes.Contains(b, []byte(name)) {
			t.Fatalf("missing %s", name)
		}
	}
	out = call(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"create_artifact","arguments":{"title":"From MCP","kind":"markdown","content":"# Hello"}}}`)
	b, _ = json.Marshal(out)
	if bytes.Contains(b, []byte(`"isError":true`)) || !bytes.Contains(b, []byte("From MCP")) {
		t.Fatalf("create failed: %s", b)
	}
	w := request(h, "GET", "/api/artifacts", "")
	if !strings.Contains(w.Body.String(), "From MCP") {
		t.Fatal("MCP storage differs from UI")
	}
}

// Through the proxy the app must advertise https everywhere it builds an absolute URL, or a
// loaded page pulls its vendor scripts over http (mixed content) and agents get an http
// upload URL that the Origin guard then refuses.
func TestProxyOriginAdvertisedAsHTTPS(t *testing.T) {
	h := testServer(t)
	proxied := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://shartifacts.example.com"+path, strings.NewReader(body))
		r.RemoteAddr = "172.18.0.1:2"
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	const base = "https://shartifacts.example.com"
	create := proxied("POST", "/api/artifacts", `{"title":"P","kind":"react","content":"export default function A(){return <h1>p</h1>}"}`)
	if create.Code != 201 {
		t.Fatalf("create through proxy: %d %s", create.Code, create.Body.String())
	}
	var a struct {
		ID string `json:"id"`
	}
	if e := json.Unmarshal(create.Body.Bytes(), &a); e != nil {
		t.Fatal(e)
	}
	w := proxied("GET", "/preview/"+a.ID, "")
	csp := w.Header().Get("Content-Security-Policy")
	if w.Code != 200 || !strings.Contains(csp, base+"/vendor/") || !strings.Contains(w.Body.String(), `src="`+base+`/vendor/react.js"`) {
		t.Fatalf("preview CSP does not use the proxy origin: %d %s", w.Code, csp)
	}
	init := proxied("POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_media","arguments":{}}}`)
	if init.Code != 200 || !strings.Contains(init.Body.String(), base+"/api/media") {
		t.Fatalf("MCP upload URL is not the proxy origin: %d %s", init.Code, init.Body.String())
	}
}
func TestReactLibraries(t *testing.T) {
	_, _, libs, e := compileReact(`import {LineChart} from 'recharts'; import * as d3 from 'd3'; import _ from 'lodash'; import debounce from 'lodash/debounce'; import Chart from 'chart.js/auto'; import {Camera} from 'lucide-react'; import {createPortal} from 'react-dom';
export default function A(){return <div>{String(!!LineChart && !!d3.select && !!_.chunk && !!debounce && !!Chart && !!Camera && !!createPortal)}</div>}`, nil)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Join(libs, ",") != "chart,d3,lodash,lucide-react,recharts" {
		t.Fatal(libs)
	}
	for _, tc := range []struct {
		source string
		want   bool
	}{
		{`export default () => <div className="flex items-center gap-4 p-6 bg-slate-100">x</div>`, true},
		{"export default () => <div className={`md:grid grid-cols-3 ${x} text-sm`}>x</div>", true},
		{`export default () => <div className="card" style={{display:'flex', border:'1px solid', padding: 8}}>x</div>`, false},
		{`export default () => <div className="artifact-page hero">x</div>`, false},
	} {
		if got := usesTailwind(tc.source); got != tc.want {
			t.Fatalf("%s: tailwind=%v", tc.source, got)
		}
	}
}
func TestLocalizeCDNScripts(t *testing.T) {
	const origin = "http://192.168.1.2:8080"
	for in, want := range map[string]string{
		`<script src="https://cdnjs.cloudflare.com/ajax/libs/Chart.js/4.4.1/chart.umd.min.js" integrity="sha512-abc" crossorigin="anonymous"></script>`: `<script src="` + origin + `/vendor/lib/chart.js" crossorigin="anonymous"></script>`,
		`<script src='https://cdn.tailwindcss.com'></script>`:                              `<script src="` + origin + `/vendor/lib/tailwind.js"></script>`,
		`<script src=https://cdn.jsdelivr.net/npm/d3@7></script>`:                          `<script src="` + origin + `/vendor/lib/d3.js"></script>`,
		`<SCRIPT defer SRC="https://unpkg.com/three@0.160.0/build/three.min.js"></SCRIPT>`: `<SCRIPT defer src="` + origin + `/vendor/lib/three.js"></SCRIPT>`,
		// Unknown libraries and module scripts stay as written (and the CSP blocks them).
		`<script src="https://cdn.example.com/x.js"></script>`:                                        `<script src="https://cdn.example.com/x.js"></script>`,
		`<script type="module" src="https://unpkg.com/three@0.160.0/build/three.module.js"></script>`: `<script type="module" src="https://unpkg.com/three@0.160.0/build/three.module.js"></script>`,
		`<script>const u="https://cdn.tailwindcss.com"</script>`:                                      `<script>const u="https://cdn.tailwindcss.com"</script>`,
	} {
		if got := localizeScripts(in, origin); got != want {
			t.Fatalf("\n in: %s\ngot: %s\nwant: %s", in, got, want)
		}
	}
}
func TestVendorRevalidates(t *testing.T) {
	st, e := store.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	h := (&Server{Store: st, Web: fstest.MapFS{"vendor/lib/x.js": {Data: []byte("x()")}}}).Handler()
	w := request(h, "GET", "/vendor/lib/x.js", "")
	tag := w.Header().Get("ETag")
	if w.Code != 200 || tag == "" || w.Header().Get("Cache-Control") != "no-cache" {
		t.Fatal(w.Code, w.Header())
	}
	r := httptest.NewRequest("GET", "http://127.0.0.1:8080/vendor/lib/x.js", nil)
	r.RemoteAddr = "192.168.1.9:1"
	r.Header.Set("If-None-Match", tag)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 304 {
		t.Fatal(w.Code)
	}
}
func TestPinAndDescription(t *testing.T) {
	h := testServer(t)
	w := request(h, "POST", "/api/artifacts", `{"title":"A","kind":"html","content":"x","description":"A tiny tool."}`)
	var a store.Artifact
	json.Unmarshal(w.Body.Bytes(), &a)
	if a.Description != "A tiny tool." {
		t.Fatal(w.Body.String())
	}
	if w = request(h, "PUT", "/api/artifacts/"+a.ID+"/pin", `{"pinned":true}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	// An update without a description keeps it; pinning added no revision.
	if w = request(h, "PUT", "/api/artifacts/"+a.ID, `{"title":"A","kind":"html","content":"y","base_version":1}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = request(h, "GET", "/api/artifacts", "")
	if !strings.Contains(w.Body.String(), `"version":2,`) || !strings.Contains(w.Body.String(), `"description":"A tiny tool.","pinned":true`) {
		t.Fatal(w.Body.String())
	}
	if w = request(h, "PUT", "/api/artifacts/nope/pin", `{"pinned":true}`); w.Code != 404 {
		t.Fatal(w.Code)
	}
}
func TestMathDetection(t *testing.T) {
	for src, want := range map[string]bool{
		"Euler: $e^{i\\pi}+1=0$":       true,
		"$$\n\\int_0^1 x\\,dx\n$$":     true,
		"inline \\(a^2+b^2\\)":         true,
		"It costs $5 and $10 at most.": false,
		"no math here":                 false,
		"a single $x$ variable":        true,
	} {
		if hasMath.MatchString(src) != want {
			t.Fatalf("%q: want %v", src, want)
		}
	}
}
