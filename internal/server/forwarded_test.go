package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/LaneBirmingham/shartifacts/internal/store"
)

// A proxy on the same machine makes every client look private to the peer allowlist. The
// headers it adds name the real client, and a public one keeps the workspace closed.
func TestForwardedPublicClients(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	web := testWeb()
	web["share.js"] = &fstest.MapFile{Data: []byte("share")}
	s := &Server{Store: st, Web: web, AllowedHosts: []string{"shartifacts.example.com"}, ShareOrigin: "https://share.example.com"}
	h := s.Handler()
	for _, tc := range []struct {
		name, host, path, header, value string
		authProxy                       bool
		want                            int
	}{
		{"no proxy", "shartifacts.example.com", "/api/artifacts", "", "", false, 200},
		{"LAN client via proxy", "shartifacts.example.com", "/api/artifacts", "X-Forwarded-For", "192.168.1.30", false, 200},
		{"tailnet client via proxy", "shartifacts.example.com", "/api/artifacts", "X-Forwarded-For", "100.101.102.103, 127.0.0.1", false, 200},
		{"tailnet v6 via proxy", "shartifacts.example.com", "/api/artifacts", "X-Forwarded-For", "fd7a:115c:a1e0::1", false, 200},
		{"public client via proxy", "shartifacts.example.com", "/api/artifacts", "X-Forwarded-For", "203.0.113.9", false, 403},
		{"public client later in chain", "shartifacts.example.com", "/", "X-Forwarded-For", "10.0.0.2, 203.0.113.9", false, 403},
		{"public client to mcp", "shartifacts.example.com", "/mcp", "X-Real-Ip", "203.0.113.9", false, 403},
		{"cloudflare tunnel", "shartifacts.example.com", "/", "Cf-Connecting-Ip", "2001:db8::7", false, 403},
		{"rfc 7239", "shartifacts.example.com", "/", "Forwarded", `for="[2001:db8::7]:4711";proto=https`, false, 403},
		{"rfc 7239 obfuscated", "shartifacts.example.com", "/api/artifacts", "Forwarded", "for=_hidden;proto=https", false, 200},
		{"mapped v4 public", "shartifacts.example.com", "/", "X-Forwarded-For", "::ffff:203.0.113.9", false, 403},
		{"health checks still answer", "shartifacts.example.com", "/healthz", "X-Forwarded-For", "203.0.113.9", false, 200},
		{"authenticating proxy", "shartifacts.example.com", "/api/artifacts", "X-Forwarded-For", "203.0.113.9", true, 200},
		// The share hostname is public by design.
		{"share page", "share.example.com", "/s/nope", "X-Forwarded-For", "203.0.113.9", false, 404},
		{"share page script", "share.example.com", "/share.js", "X-Forwarded-For", "203.0.113.9", false, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s.AuthProxy = tc.authProxy
			r := httptest.NewRequest("GET", "http://"+tc.host+tc.path, nil)
			r.RemoteAddr = "172.18.0.1:5000"
			if tc.header != "" {
				r.Header.Set(tc.header, tc.value)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

// Caddy's on-demand TLS asks before getting a certificate: only the share hostname and the
// hostnames of apps that exist may have one.
func TestTLSAllowed(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	s := &Server{Store: st, Web: testWeb(), ShareOrigin: "https://share.example.com", AppOrigin: "https://*.share.example.com"}
	h := s.Handler()
	var created struct {
		App struct {
			ID string `json:"id"`
		} `json:"app"`
	}
	json.Unmarshal(call(h, "POST", "/api/apps", `{"name":"Budget","label":"budget","files":{"index.html":"<h1>hi</h1>"}}`).Body.Bytes(), &created)
	id := created.App.ID
	if id == "" {
		t.Fatal("no app")
	}
	a, _ := st.App(id)
	ask := func(host, domain string, headers ...string) int {
		r := httptest.NewRequest("GET", "http://"+host+TLSAllowedPath+"?domain="+domain, nil)
		r.RemoteAddr = "172.18.0.5:5000"
		for i := 0; i+1 < len(headers); i += 2 {
			r.Header.Set(headers[i], headers[i+1])
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for _, tc := range []struct {
		domain string
		want   int
	}{
		{"share.example.com", 200},
		{"budget.share.example.com", 200},
		{"BUDGET.share.example.com.", 200},
		{s.generatedLabel(a.ID) + ".share.example.com", 200},
		{"nobody.share.example.com", 404},
		{s.generatedLabel("missing") + ".share.example.com", 404},
		{"a.budget.share.example.com", 404},
		{"shartifacts.example.com", 404},
		{"example.com", 404},
		{"", 404},
	} {
		// Caddy calls by its own name for this server, which is no allowed host.
		if got := ask("shartifacts:8080", tc.domain); got != tc.want {
			t.Errorf("%q: %d, want %d", tc.domain, got, tc.want)
		}
	}
	// Not through the proxy, and not from the internet.
	if got := ask("share.example.com", "budget.share.example.com"); got != 404 {
		t.Errorf("answered on the share hostname: %d", got)
	}
	if got := ask("shartifacts:8080", "budget.share.example.com", "X-Forwarded-For", "192.168.1.4"); got != 404 {
		t.Errorf("answered a forwarded request: %d", got)
	}
	r := httptest.NewRequest("GET", "http://shartifacts:8080"+TLSAllowedPath+"?domain=share.example.com", nil)
	r.RemoteAddr = "203.0.113.9:5000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Errorf("answered a public peer: %d", w.Code)
	}
}

// The public listener is what the edge proxy forwards to. Whatever Host the proxy sends
// (nginx's default is its upstream address), it serves share and app links and nothing else.
func TestPublicListener(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	web := testWeb()
	web["share.js"] = &fstest.MapFile{Data: []byte("share")}
	s := &Server{Store: st, Web: web, ShareOrigin: "https://share.example.com", AppOrigin: "https://*.share.example.com"}
	h, public := s.Handler(), s.PublicHandler()
	var a store.Artifact
	json.Unmarshal(request(h, "POST", "/api/artifacts", `{"title":"Hi","kind":"html","content":"<h1>hi</h1>"}`).Body.Bytes(), &a)
	var link map[string]any
	json.Unmarshal(request(h, "POST", "/api/artifacts/"+a.ID+"/shares", `{"version":0}`).Body.Bytes(), &link)
	token, _ := link["token"].(string)
	get := func(method, host, path, peer string) int {
		r := httptest.NewRequest(method, "http://"+host+path, nil)
		r.RemoteAddr = peer
		w := httptest.NewRecorder()
		public.ServeHTTP(w, r)
		return w.Code
	}
	for _, host := range []string{"127.0.0.1:8081", "localhost", "shartifacts:8081", "share.example.com"} {
		for _, path := range []string{"/", "/artifacts", "/a/" + a.ID, "/api/artifacts", "/api/config", "/healthz", "/media/x", "/preview/" + a.ID} {
			if got := get("GET", host, path, "172.18.0.3:5000"); got != 404 {
				t.Errorf("%s%s: %d, want 404", host, path, got)
			}
		}
		if got := get("POST", host, "/mcp", "172.18.0.3:5000"); got != 404 {
			t.Errorf("%s POST /mcp: %d", host, got)
		}
		if got := get("GET", host, "/s/"+token, "203.0.113.9:5000"); got != 200 {
			t.Errorf("%s share link: %d", host, got)
		}
	}
	// An app's hostname still reaches the app: its link's token is the grant.
	if got := get("GET", "nobody.share.example.com", "/p/0123456789abcdef0123456789abcdef/", "172.18.0.3:5000"); got == 200 {
		t.Errorf("unknown app hostname served: %d", got)
	}
}
