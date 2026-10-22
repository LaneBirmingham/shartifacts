package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/LaneBirmingham/shartifacts/internal/store"
)

func TestShareLinks(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	s := &Server{Store: st, Web: testWeb(), ShareOrigin: "https://share.example.com"}
	h := s.Handler()
	w := request(h, "POST", "/api/artifacts", `{"title":"Board","kind":"html","content":"<!doctype html><h1>one</h1>","capabilities":{"db":{"rules":[{"path":"team","read":"interact"}]}}}`)
	var a store.Artifact
	json.Unmarshal(w.Body.Bytes(), &a)
	st.DocWrite(a.ID, "set", "tasks/t1", map[string]any{"title": "public"}, store.Viewer{Owner: true})
	st.DocWrite(a.ID, "set", "team/notes", map[string]any{"title": "members only"}, store.Viewer{Owner: true})
	st.DocWrite(a.ID, "set", "data/users/u_x/prefs", map[string]any{"title": "private"}, store.Viewer{ID: "u_x"})
	st.StorageSet(a.ID, "count", "7", true)
	st.StorageSet(a.ID, "count", "owner only", false)
	var pinned, latest map[string]any
	json.Unmarshal(request(h, "POST", "/api/artifacts/"+a.ID+"/shares", `{"version":1}`).Body.Bytes(), &pinned)
	json.Unmarshal(request(h, "POST", "/api/artifacts/"+a.ID+"/shares", `{"version":0}`).Body.Bytes(), &latest)
	if !strings.HasPrefix(pinned["url"].(string), "https://share.example.com/s/") {
		t.Fatal(pinned)
	}
	request(h, "PUT", "/api/artifacts/"+a.ID, `{"content":"<!doctype html><h1>two</h1>","base_version":1}`)
	pin, last := pinned["token"].(string), latest["token"].(string)
	// What the share hostname answers, as the reverse proxy would deliver it.
	public := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://share.example.com"+path, strings.NewReader(body))
		r.RemoteAddr = "172.18.0.1:4000"
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/", "/a/" + a.ID, "/view/" + a.ID, "/preview/" + a.ID, "/api/artifacts", "/api/artifacts/" + a.ID, "/api/people", "/api/config", "/api/media", "/app.js", "/index.html", "/healthz", "/draft-preview/x"} {
		if w := public("GET", path, ""); w.Code != 404 {
			t.Fatalf("share host served %s: %d", path, w.Code)
		}
	}
	for _, path := range []string{"/mcp", "/api/artifacts", "/api/artifacts/" + a.ID + "/db", "/api/artifacts/" + a.ID + "/storage", "/api/media", "/api/sample", "/s/" + pin + "/api/me"} {
		if w := public("POST", path, `{}`); w.Code != 404 {
			t.Fatalf("share host accepted POST %s: %d", path, w.Code)
		}
	}
	// The read-only data calls find the link before reading a body, and read only a small one.
	big := `{"op":"get","path":"tasks/t1","data":"` + strings.Repeat("x", 100<<10) + `"}`
	if w := public("POST", "/s/"+strings.Repeat("0", 32)+"/api/db", big); w.Code != 404 {
		t.Fatalf("unknown link read a body: %d", w.Code)
	}
	if w := public("POST", "/s/"+pin+"/api/db", big); w.Code != 400 {
		t.Fatalf("oversized share body: %d", w.Code)
	}
	if w := public("GET", "/s/"+pin, ""); w.Code != 200 || !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatal(w.Code, w.Header())
	}
	if w := public("GET", "/s/"+pin+"/preview", ""); !strings.Contains(w.Body.String(), "<h1>one</h1>") || !strings.Contains(w.Header().Get("Content-Security-Policy"), "https://share.example.com/vendor/") {
		t.Fatal("pinned link does not serve its version over https", w.Header().Get("Content-Security-Policy"))
	}
	if w := public("GET", "/s/"+last+"/preview", ""); !strings.Contains(w.Body.String(), "<h1>two</h1>") {
		t.Fatal("latest link does not follow new versions")
	}
	db := func(body string) (int, string) {
		w := public("POST", "/s/"+pin+"/api/db", body)
		return w.Code, w.Body.String()
	}
	if code, body := db(`{"op":"get","path":"tasks/t1"}`); code != 200 || !strings.Contains(body, "public") {
		t.Fatal(code, body)
	}
	for path, hidden := range map[string]string{"team/notes": "members only", "data/users/u_x/prefs": "private"} {
		if _, body := db(`{"op":"get","path":"` + path + `"}`); strings.Contains(body, hidden) {
			t.Fatalf("share visitor read %s", path)
		}
	}
	if code, _ := db(`{"op":"set","path":"tasks/t1","data":{"title":"defaced"}}`); code != 403 {
		t.Fatal("share visitor wrote", code)
	}
	if w := public("POST", "/s/"+pin+"/api/storage", `{"op":"get","key":"count","shared":true}`); !strings.Contains(w.Body.String(), `"value":"7"`) {
		t.Fatal(w.Body.String())
	}
	// Personal storage is the owner's own; a visitor has none.
	if w := public("POST", "/s/"+pin+"/api/storage", `{"op":"get","key":"count"}`); strings.TrimSpace(w.Body.String()) != "null" {
		t.Fatal("share visitor read personal storage:", w.Body.String())
	}
	if w := public("POST", "/s/"+pin+"/api/storage", `{"op":"set","key":"count","value":"0"}`); w.Code != 403 {
		t.Fatal("share visitor wrote storage")
	}
	if w := public("GET", "/s/"+pin+"/api/me", ""); !strings.Contains(w.Body.String(), `"id":null`) || !strings.Contains(w.Body.String(), `"canEdit":false`) {
		t.Fatal(w.Body.String())
	}
	request(h, "DELETE", "/api/shares/"+pin, "")
	if w := public("GET", "/s/"+pin, ""); w.Code != 404 {
		t.Fatal("revoked link still works")
	}
	// A link names its version, or 0 to follow new versions; never by omission.
	if w := request(h, "POST", "/api/artifacts/"+a.ID+"/shares", `{}`); w.Code != 400 {
		t.Fatal("share created without a version:", w.Code)
	}
	// Without a share origin, links cannot be made.
	off := (&Server{Store: st, Web: testWeb()}).Handler()
	if w := request(off, "POST", "/api/artifacts/"+a.ID+"/shares", `{"version":0}`); w.Code != 400 {
		t.Fatal("share created with sharing off")
	}
}

func testWeb() fstest.MapFS {
	return fstest.MapFS{"index.html": {Data: []byte("hello")}, "share.html": {Data: []byte("share")}, "app.js": {Data: []byte("app")}}
}
