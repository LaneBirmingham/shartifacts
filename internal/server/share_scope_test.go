package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LaneBirmingham/shartifacts/internal/store"
)

// TestShareTokenScope holds the share hostname to its authorisation model: a visitor reads
// nothing of any artifact without a token, and with token A only what A shares. Everything
// that must stay out of reach carries a marker; no response on the share hostname, to any
// request we can think to send, may contain one.
func TestShareTokenScope(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	s := &Server{Store: st, Web: testWeb(), ShareOrigin: "https://share.example.com"}
	h := s.Handler()
	owner := store.Viewer{Owner: true}
	media := func(marker string) string {
		m, e := st.SaveMedia("image/png", strings.NewReader("\x89PNG\r\n\x1a\n"+marker))
		if e != nil {
			t.Fatal(e)
		}
		return m.ID
	}
	create := func(body string) store.Artifact {
		var a store.Artifact
		json.Unmarshal(request(h, "POST", "/api/artifacts", body).Body.Bytes(), &a)
		return a
	}

	// B: never to be read with A's token, though it has a link of its own.
	mediaB := media("B-MEDIA")
	b := create(`{"title":"B-TITLE","kind":"html","content":"<!doctype html><p>B-CONTENT</p><img src=\"/media/` + mediaB + `\">","files":{"b.css":"/* B-FILE */","index.html":"<p>B-PAGE</p>"},"description":"B-DESCRIPTION"}`)
	assetB, _ := st.UploadAsset(b.ID, "image/png", "", strings.NewReader("\x89PNG\r\n\x1a\nB-ASSET"))
	st.DocWrite(b.ID, "set", "tasks/t1", map[string]any{"t": "B-DOC"}, owner)
	st.StorageSet(b.ID, "k", "B-SHARED-STORAGE", true)
	st.StorageSet(b.ID, "k", "B-PERSONAL-STORAGE", false)
	tokenB := share(t, h, b.ID, 0)

	// A: shared at version 1; what follows is not part of that link.
	mediaA := media("A-MEDIA")
	a := create(`{"title":"A","kind":"html","content":"<!doctype html><p>A-CONTENT</p><img src=\"/media/` + mediaA + `\"><script>const u='/_blob/'</script>","capabilities":{"db":{"rules":[{"path":"team","read":"interact"}]}}}`)
	assetA, _ := st.UploadAsset(a.ID, "image/png", "", strings.NewReader("\x89PNG\r\n\x1a\nA-ASSET"))
	st.DocWrite(a.ID, "set", "tasks/t1", map[string]any{"t": "A-DOC", "img": assetA.URL}, owner)
	st.DocWrite(a.ID, "set", "team/notes", map[string]any{"t": "A-MEMBERS-ONLY"}, owner)
	st.DocWrite(a.ID, "set", "data/users/u_lane/diary", map[string]any{"t": "A-PRIVATE-DOC"}, store.Viewer{ID: "u_lane"})
	st.StorageSet(a.ID, "k", "A-SHARED-STORAGE", true)
	st.StorageSet(a.ID, "k", "A-PERSONAL-STORAGE", false)
	tokenA := share(t, h, a.ID, 1)
	mediaA2 := media("A-V2-MEDIA")
	request(h, "PUT", "/api/artifacts/"+a.ID, `{"base_version":1,"content":"<!doctype html><p>A-V2-CONTENT</p><img src=\"/media/`+mediaA2+`\">","description":"A-V2-DESCRIPTION"}`)

	// Never readable with token A (or without a token), and never readable at all here.
	forbiddenWithA := []string{"B-TITLE", "B-CONTENT", "B-PAGE", "B-FILE", "B-DESCRIPTION", "B-MEDIA", "B-ASSET", "B-DOC", "B-SHARED-STORAGE", "B-PERSONAL-STORAGE",
		"A-MEMBERS-ONLY", "A-PRIVATE-DOC", "A-PERSONAL-STORAGE", "A-V2-CONTENT", "A-V2-MEDIA", "A-V2-DESCRIPTION"}
	sharedByA := []string{"A-CONTENT", "A-MEDIA", "A-ASSET", "A-DOC", "A-SHARED-STORAGE"}

	public := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://share.example.com"+path, strings.NewReader(body))
		r.RemoteAddr = "172.18.0.1:4000"
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	// Every read we can name, under every token prefix: none, A's, B's, and a made-up one.
	reads := func(prefix string) (gets []string, posts [][2]string) {
		for _, m := range []string{mediaA, mediaA2, mediaB} {
			gets = append(gets, prefix+"/media/"+m, prefix+"/preview/../media/"+m)
		}
		for _, x := range []string{assetA.ID, assetB.ID} {
			gets = append(gets, prefix+"/_blob/"+x, prefix+"/preview/../_blob/"+x)
		}
		gets = append(gets, prefix, prefix+"/", prefix+"/meta", prefix+"/preview", prefix+"/preview/", prefix+"/preview/b.css", prefix+"/preview/index.html", prefix+"/api/me")
		for _, p := range []string{"tasks/t1", "team/notes", "data/users/u_lane/diary"} {
			posts = append(posts, [2]string{prefix + "/api/db", `{"op":"get","path":"` + p + `"}`})
		}
		for _, c := range []string{"tasks", "team", "data/users/u_lane"} {
			posts = append(posts, [2]string{prefix + "/api/db", `{"op":"query","path":"` + c + `"}`})
		}
		for _, shared := range []string{"true", "false"} {
			posts = append(posts, [2]string{prefix + "/api/storage", `{"op":"get","key":"k","shared":` + shared + `}`}, [2]string{prefix + "/api/storage", `{"op":"list","shared":` + shared + `}`})
		}
		return
	}
	bodies := func(prefix string) string {
		var all strings.Builder
		gets, posts := reads(prefix)
		for _, p := range gets {
			w := public("GET", p, "")
			if w.Code >= 300 && w.Code < 400 {
				// Follow redirects (directory and path cleaning) as a browser would.
				w = public("GET", w.Header().Get("Location"), "")
			}
			all.WriteString(w.Body.String())
		}
		for _, p := range posts {
			all.WriteString(public("POST", p[0], p[1]).Body.String())
		}
		return all.String()
	}

	// No token: nothing of either artifact.
	// The near miss differs from token B in its last character, whatever that is.
	nearB := tokenB[:31] + map[bool]string{true: "1", false: "0"}[tokenB[31] == '0']
	for _, prefix := range []string{"", "/s/0123456789abcdef0123456789abcdef", "/s/" + nearB} {
		got := bodies(prefix)
		for _, m := range append(forbiddenWithA, sharedByA...) {
			if strings.Contains(got, m) {
				t.Errorf("share host served %s with no valid token (prefix %q)", m, prefix)
			}
		}
	}
	// Token A: what A shares, and nothing else.
	got := bodies("/s/" + tokenA)
	for _, m := range forbiddenWithA {
		if strings.Contains(got, m) {
			t.Errorf("token A read %s", m)
		}
	}
	for _, m := range sharedByA {
		if !strings.Contains(got, m) {
			t.Errorf("token A could not read %s, which it shares", m)
		}
	}
	// B's own link still works for B (the scoping is per token, not a blanket refusal).
	if w := public("GET", "/s/"+tokenB+"/media/"+mediaB, ""); !strings.Contains(w.Body.String(), "B-MEDIA") {
		t.Error("token B cannot read B's media", w.Code)
	}
	// The shared page loads its objects through the token's paths, and is allowed only those.
	w := public("GET", "/s/"+tokenA+"/preview", "")
	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(w.Body.String(), `src="/s/`+tokenA+`/media/`+mediaA+`"`) || !strings.Contains(w.Body.String(), `'/s/`+tokenA+`/_blob/'`) ||
		!strings.Contains(csp, "https://share.example.com/s/"+tokenA+"/media/") || strings.Contains(csp, "https://share.example.com/media/") || strings.Contains(csp, "https://share.example.com/_blob/") {
		t.Fatal("shared page not scoped to its token:", csp, w.Body.String())
	}
	if w := public("POST", "/s/"+tokenA+"/api/db", `{"op":"get","path":"tasks/t1"}`); !strings.Contains(w.Body.String(), `"/s/`+tokenA+`/_blob/`+assetA.ID+`"`) {
		t.Error("asset URLs in shared data are not scoped to the token:", w.Body.String())
	}
	// Revoking ends every read.
	request(h, "DELETE", "/api/shares/"+tokenA, "")
	got = bodies("/s/" + tokenA)
	for _, m := range sharedByA {
		if strings.Contains(got, m) {
			t.Errorf("revoked token A still reads %s", m)
		}
	}
	// The workspace keeps serving its objects at their own paths.
	for _, p := range []string{"/media/" + mediaB, "/_blob/" + assetB.ID} {
		if w := request(h, "GET", p, ""); w.Code != 200 {
			t.Errorf("workspace lost %s: %d", p, w.Code)
		}
	}
}

// TestShareLiveScope: a visitor's live stream carries notice only of what they could read.
func TestShareLiveScope(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	s := &Server{Store: st, Web: testWeb(), ShareOrigin: "https://share.example.com"}
	h := s.Handler()
	srv := httptest.NewServer(h)
	defer srv.Close()
	var a, b store.Artifact
	json.Unmarshal(request(h, "POST", "/api/artifacts", `{"title":"A","kind":"html","content":"<!doctype html>a","capabilities":{"db":{"rules":[{"path":"team","read":"interact"}]}}}`).Body.Bytes(), &a)
	json.Unmarshal(request(h, "POST", "/api/artifacts", `{"title":"B","kind":"html","content":"<!doctype html>b"}`).Body.Bytes(), &b)
	pinned := share(t, h, a.ID, 1)
	req, _ := http.NewRequest("GET", srv.URL+"/s/"+pinned+"/api/live?peer=visitor0001", nil)
	req.Host = "share.example.com"
	res, e := http.DefaultClient.Do(req)
	if e != nil || res.StatusCode != 200 {
		t.Fatal(e, res)
	}
	defer res.Body.Close()
	lines := bufio.NewReader(res.Body)
	if l, _ := lines.ReadString('\n'); !strings.Contains(l, "hello") {
		t.Fatal(l)
	}
	owner := store.Viewer{Owner: true}
	st.DocWrite(a.ID, "set", "data/users/u_lane/diary-about-sam", map[string]any{"t": 1}, store.Viewer{ID: "u_lane"})
	st.DocWrite(a.ID, "set", "team/secret-plans", map[string]any{"t": 1}, owner)
	st.DocWrite(b.ID, "set", "tasks/b-task", map[string]any{"t": 1}, owner)
	request(h, "PUT", "/api/artifacts/"+a.ID, `{"base_version":1,"content":"<!doctype html>v2"}`)
	st.DocWrite(a.ID, "set", "tasks/visible", map[string]any{"t": 1}, owner)
	done := make(chan string)
	go func() {
		var seen strings.Builder
		for {
			l, e := lines.ReadString('\n')
			seen.WriteString(l)
			if e != nil || strings.Contains(l, "tasks/visible") {
				done <- seen.String()
				return
			}
		}
	}()
	select {
	case seen := <-done:
		for _, m := range []string{"u_lane", "diary-about-sam", "secret-plans", "b-task", `"version"`} {
			if strings.Contains(seen, m) {
				t.Errorf("pinned visitor heard %s: %s", m, seen)
			}
		}
		if !strings.Contains(seen, "tasks/visible") {
			t.Error("visitor missed a change to shared data")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event for shared data")
	}
}

func share(t *testing.T, h http.Handler, id string, version int) string {
	t.Helper()
	w := request(h, "POST", "/api/artifacts/"+id+"/shares", fmt.Sprintf(`{"version":%d}`, version))
	var sh struct{ Token string }
	json.Unmarshal(w.Body.Bytes(), &sh)
	if sh.Token == "" {
		t.Fatal(w.Code, w.Body.String())
	}
	return sh.Token
}
