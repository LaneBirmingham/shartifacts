package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LaneBirmingham/shartifacts/internal/search"
	"github.com/LaneBirmingham/shartifacts/internal/store"
)

func fileServer(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	st, e := store.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	s := &Server{Store: st, Web: testWeb(), ShareOrigin: "https://share.example.com"}
	return s.Handler(), st
}

func upload(t *testing.T, h http.Handler, typ, name string, body []byte) store.Media {
	t.Helper()
	r := httptest.NewRequest("POST", "http://127.0.0.1:8080/api/media?name="+name, bytes.NewReader(body))
	r.RemoteAddr = "192.168.1.9:1234"
	r.Header.Set("Content-Type", typ)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	var m store.Media
	if e := json.Unmarshal(w.Body.Bytes(), &m); e != nil {
		t.Fatal(e)
	}
	return m
}

func saveFile(t *testing.T, h http.Handler, title, url string) store.Artifact {
	t.Helper()
	w := request(h, "POST", "/api/artifacts", fmt.Sprintf(`{"title":%q,"kind":"file","content":%q}`, title, url))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	var a store.Artifact
	if e := json.Unmarshal(w.Body.Bytes(), &a); e != nil {
		t.Fatal(e)
	}
	return a
}

// fromShare asks as the reverse proxy delivers a visitor's request to the share hostname.
func fromShare(h http.Handler, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "http://share.example.com"+path, nil)
	r.RemoteAddr = "172.18.0.1:4000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func shareToken(t *testing.T, h http.Handler, id string, version int) string {
	t.Helper()
	var sh map[string]any
	w := request(h, "POST", "/api/artifacts/"+id+"/shares", fmt.Sprintf(`{"version":%d}`, version))
	if e := json.Unmarshal(w.Body.Bytes(), &sh); e != nil || w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	return sh["token"].(string)
}

func TestFileArtifactDownloads(t *testing.T) {
	h, _ := fileServer(t)
	zip := []byte("PK\x03\x04 a zip, as far as this test cares")
	m := upload(t, h, "application/x-zip-compressed", "bundle.zip", zip)
	if m.MIMEType != "application/zip" || !strings.HasSuffix(m.ID, ".zip") {
		t.Fatal("zip alias not recognised", m)
	}
	a := saveFile(t, h, "The bundle", m.URL)
	f, e := store.ParseFileRef(a.Revisions[0].Content)
	if e != nil || f.Name != "bundle.zip" || f.Size != int64(len(zip)) || f.MIMEType != "application/zip" {
		t.Fatal(f, e)
	}

	// The workspace's Download menu entry gives the file itself, under its own name.
	w := request(h, "GET", "/api/artifacts/"+a.ID+"/download", "")
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), zip) {
		t.Fatal(w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Disposition"); got != `attachment; filename=bundle.zip` {
		t.Fatal(got)
	}
	if w.Header().Get("Content-Security-Policy") != "default-src 'none'; sandbox" {
		t.Fatal(w.Header())
	}

	// The preview is a sandboxed page with a link it can follow from its opaque origin, and
	// it does not leak the media URL.
	w = request(h, "GET", "/preview/"+a.ID, "")
	link := fmt.Sprintf(`href="/preview/%s/1/download"`, a.ID)
	if body := w.Body.String(); !strings.Contains(body, link) || !strings.Contains(body, "The bundle") || strings.Contains(body, "/media/") {
		t.Fatal(body)
	}
	r := httptest.NewRequest("GET", "http://127.0.0.1:8080/preview/"+a.ID+"/1/download", nil)
	r.RemoteAddr = "192.168.1.9:1234"
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	r.Header.Set("Sec-Fetch-Mode", "navigate")
	r.Header.Set("Sec-Fetch-Dest", "iframe")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), zip) {
		t.Fatal("preview's link refused", rec.Code, rec.Body.String())
	}

	// The page follows the theme it is told, by its parent only, and the system's otherwise.
	body := w.Body.String()
	for _, want := range []string{"shartifacts-theme-request", "e.source!==parent", "color-scheme:light dark", ":root[data-scheme=dark]", "light-dark(#f9f8f6,#1c1a19)"} {
		if !strings.Contains(body, want) {
			t.Fatal("missing", want)
		}
	}

	// A dropped download resumes.
	r = httptest.NewRequest("GET", "http://127.0.0.1:8080/api/artifacts/"+a.ID+"/download", nil)
	r.RemoteAddr = "192.168.1.9:1234"
	r.Header.Set("Range", "bytes=0-1")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 206 || rec.Body.String() != "PK" {
		t.Fatal(rec.Code, rec.Body.String())
	}

	// Only a file artifact answers at download under /preview/: another artifact's files
	// keep their paths, even one that is called download.
	w = request(h, "POST", "/api/artifacts", `{"title":"Site","kind":"html","content":"<h1>hi</h1>","files":{"download":"its own file"}}`)
	var site store.Artifact
	json.Unmarshal(w.Body.Bytes(), &site)
	w = request(h, "GET", "/preview/"+site.ID+"/1/download", "")
	if w.Body.String() != "its own file" || w.Header().Get("Content-Disposition") != "" {
		t.Fatal(w.Header(), w.Body.String())
	}
}

// Anything can be shared, and nothing shared can run: it arrives as bytes to save.
func TestFileArtifactNeverRuns(t *testing.T) {
	h, _ := fileServer(t)
	page := []byte("<script>alert(document.domain)</script>")
	// Refused as what it claims to be.
	for _, typ := range []string{"text/html", "image/svg+xml", "application/xhtml+xml", "application/x-www-form-urlencoded"} {
		r := httptest.NewRequest("POST", "http://127.0.0.1:8080/api/media", bytes.NewReader(page))
		r.RemoteAddr = "192.168.1.9:1234"
		r.Header.Set("Content-Type", typ)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatal(typ, w.Code)
		}
	}
	// Accepted as bytes.
	m := upload(t, h, "application/octet-stream", "page.html", page)
	a := saveFile(t, h, `<img src=x onerror=alert(1)>`, m.URL)
	token := shareToken(t, h, a.ID, 0)
	for name, w := range map[string]*httptest.ResponseRecorder{
		"media":    request(h, "GET", m.URL, ""),
		"download": request(h, "GET", "/api/artifacts/"+a.ID+"/download", ""),
		"shared":   fromShare(h, "/s/"+token+"/download"),
	} {
		if w.Code != 200 || w.Header().Get("Content-Type") != "application/octet-stream" || w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Content-Security-Policy") != "default-src 'none'; sandbox" {
			t.Fatal(name, w.Code, w.Header())
		}
	}
	// The title is escaped on the page that offers it.
	if body := request(h, "GET", "/preview/"+a.ID, "").Body.String(); strings.Contains(body, "<img src=x") {
		t.Fatal(body)
	}
}

func TestFileArtifactContent(t *testing.T) {
	h, _ := fileServer(t)
	m := upload(t, h, "application/zip", "one.zip", []byte("one"))
	missing := strings.Repeat("a", 64) + ".zip"
	for name, body := range map[string]string{
		"not uploaded": `{"title":"x","kind":"file","content":"/media/` + missing + `"}`,
		"not a file":   `{"title":"x","kind":"file","content":"<h1>source</h1>"}`,
		"escaping id":  `{"title":"x","kind":"file","content":"/media/../../etc/passwd"}`,
		"with files":   `{"title":"x","kind":"file","content":"` + m.URL + `","files":{"a.css":"a{}"}}`,
	} {
		if w := request(h, "POST", "/api/artifacts", body); w.Code != 400 {
			t.Fatal(name, w.Code, w.Body.String())
		}
	}

	// A caller names the file, but cannot misstate its size or type, or reach above its name.
	w := request(h, "POST", "/api/artifacts", fmt.Sprintf(`{"title":"x","kind":"file","content":%q}`,
		fmt.Sprintf(`{"url":%q,"name":"../../evil.exe","size":1,"mime_type":"text/html"}`, m.URL)))
	var a store.Artifact
	json.Unmarshal(w.Body.Bytes(), &a)
	f, e := store.ParseFileRef(a.Revisions[0].Content)
	if e != nil || f.Name != "evil.exe" || f.Size != 3 || f.MIMEType != "application/zip" {
		t.Fatal(f, e, w.Body.String())
	}

	// Non-ASCII names survive the trip into the header.
	u := upload(t, h, "application/zip", "r%C3%A9sum%C3%A9.zip", []byte("uni"))
	w = request(h, "GET", "/api/artifacts/"+saveFile(t, h, "CV", u.URL).ID+"/download", "")
	if got := w.Header().Get("Content-Disposition"); got != `attachment; filename*=utf-8''r%C3%A9sum%C3%A9.zip` {
		t.Fatal(got)
	}

	// With no name uploaded, the title names it, and still opens as what it is.
	noName := upload(t, h, "application/gzip", "", []byte("gz"))
	a = saveFile(t, h, "Backup", noName.URL)
	if f, _ = store.ParseFileRef(a.Revisions[0].Content); f.Name != "Backup.gz" {
		t.Fatal(f)
	}

	// Neither a file nor source turns into the other.
	w = request(h, "PUT", "/api/artifacts/"+a.ID, `{"kind":"html","content":"<h1>hi</h1>","base_version":1}`)
	if w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = request(h, "POST", "/api/artifacts", `{"title":"page","kind":"html","content":"<h1>hi</h1>"}`)
	var page store.Artifact
	json.Unmarshal(w.Body.Bytes(), &page)
	w = request(h, "PUT", "/api/artifacts/"+page.ID, fmt.Sprintf(`{"kind":"file","content":%q,"base_version":1}`, m.URL))
	if w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	// Renaming keeps the file.
	w = request(h, "PUT", "/api/artifacts/"+a.ID, `{"title":"Renamed","base_version":1}`)
	json.Unmarshal(w.Body.Bytes(), &a)
	if w.Code != 200 || a.Revisions[1].Content != a.Revisions[0].Content {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestFileArtifactShareScope(t *testing.T) {
	h, st := fileServer(t)
	one := upload(t, h, "application/zip", "one.zip", []byte("first file"))
	two := upload(t, h, "application/zip", "two.zip", []byte("second file"))
	other := upload(t, h, "application/zip", "other.zip", []byte("somebody else's"))
	a := saveFile(t, h, "Release", one.URL)
	o := saveFile(t, h, "Other", other.URL)
	pinned, follows, otherToken := shareToken(t, h, a.ID, 1), shareToken(t, h, a.ID, 0), shareToken(t, h, o.ID, 0)

	w := request(h, "PUT", "/api/artifacts/"+a.ID, fmt.Sprintf(`{"content":%q,"base_version":1}`, two.URL))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	for token, want := range map[string]string{pinned: "first file", follows: "second file", otherToken: "somebody else's"} {
		w := fromShare(h, "/s/"+token+"/download")
		if w.Code != 200 || w.Body.String() != want || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment") {
			t.Fatal(w.Code, w.Body.String(), want)
		}
	}
	// A shared page points at its token, never at the workspace's paths.
	if body := fromShare(h, "/s/"+pinned+"/preview").Body.String(); !strings.Contains(body, `href="/s/`+pinned+`/download"`) || strings.Contains(body, "/media/") {
		t.Fatal(body)
	}

	// Refused: no token, a made-up one, the workspace's own paths on the share hostname, a
	// file the token's revision does not name.
	for _, path := range []string{
		"/download",
		"/s/" + strings.Repeat("0", 32) + "/download",
		one.URL,
		"/api/artifacts/" + a.ID + "/download",
		"/preview/" + a.ID + "/1/download",
		"/s/" + pinned + "/media/" + two.ID,
		"/s/" + pinned + "/media/" + other.ID,
		"/s/" + otherToken + "/media/" + one.ID,
	} {
		if w := fromShare(h, path); w.Code != 404 {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
	// A token reaches the file its revision names.
	if w := fromShare(h, "/s/"+pinned+"/media/"+one.ID); w.Code != 200 {
		t.Fatal(w.Code)
	}

	// Only a file artifact downloads at this path: a page's token shows its page.
	w = request(h, "POST", "/api/artifacts", `{"title":"page","kind":"html","content":"<h1>secret source</h1>"}`)
	var page store.Artifact
	json.Unmarshal(w.Body.Bytes(), &page)
	if w := fromShare(h, "/s/"+shareToken(t, h, page.ID, 0)+"/download"); w.Code != 404 {
		t.Fatal(w.Code, w.Body.String())
	}

	// Revoking ends the download.
	if w := request(h, "DELETE", "/api/shares/"+follows, ""); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := fromShare(h, "/s/"+follows+"/download"); w.Code != 404 {
		t.Fatal(w.Code)
	}

	// A file deleted from the Media library says so, and a title can still change.
	if e := st.DeleteMedia(one.ID); e != nil {
		t.Fatal(e)
	}
	if w := fromShare(h, "/s/"+pinned+"/download"); w.Code != 404 {
		t.Fatal(w.Code)
	}
	if body := fromShare(h, "/s/"+pinned+"/preview").Body.String(); !strings.Contains(body, "has been deleted") || strings.Contains(body, `class="get"`) {
		t.Fatal(body)
	}
}

func TestFileArtifactIsMediaInUse(t *testing.T) {
	h, st := fileServer(t)
	m := upload(t, h, "application/octet-stream", "tool.bin", []byte("bytes"))
	a := saveFile(t, h, "Tool", m.URL)
	media, e := st.ListMedia()
	if e != nil {
		t.Fatal(e)
	}
	users, e := st.MediaUsage(media)
	if e != nil || len(users[m.ID]) != 1 || users[m.ID][0].ID != a.ID {
		t.Fatal(users, e)
	}
}

func TestFileArtifactSearch(t *testing.T) {
	doc := search.NewDoc(search.Source{ID: "f", Kind: "file", Title: "Holiday photos", Content: `{"url":"/media/` + strings.Repeat("a", 64) + `.zip","name":"kangaroos.zip","size":3,"mime_type":"application/zip"}`})
	if len(doc.Traits) != 0 {
		t.Fatal("a file's own URL made it look like an image", doc.Traits)
	}
	ix := search.Build([]*search.Doc{doc})
	find := func(q string) int {
		return len(ix.Search(search.Parse(q, time.Now()), map[string]search.Meta{"f": {}}, search.Options{}).Hits)
	}
	for _, q := range []string{"kangaroos", "type:zip", "type:file"} {
		if find(q) != 1 {
			t.Fatal(q)
		}
	}
	// Only the name is words; the rest of the reference is not something people search for.
	if find("application") != 0 {
		t.Fatal("found by its stored type")
	}
}

func TestShareFromAgent(t *testing.T) {
	const origin = "http://192.168.1.9:8080"
	call := func(h http.Handler, id int, tool, args string) map[string]any {
		t.Helper()
		res := mcpRequest(t, h, origin, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, id, tool, args))
		if out, ok := res["structuredContent"].(map[string]any); ok {
			return out
		}
		t.Fatal(res)
		return nil
	}
	h, _ := fileServer(t)
	m := upload(t, h, "application/zip", "kit.zip", []byte("kit"))
	made := call(h, 1, "create_artifact", fmt.Sprintf(`{"title":"Kit","kind":"file","content":%q,"share":true}`, m.URL))
	link, _ := made["share_url"].(string)
	if !strings.HasPrefix(link, "https://share.example.com/s/") || made["download_url"] != link+"/download" {
		t.Fatal(made)
	}
	// Asking again finds the same link and adds no version.
	again := call(h, 2, "update_artifact", fmt.Sprintf(`{"id":%q,"share":true}`, made["id"]))
	if again["share_url"] != link || again["version"] != float64(1) {
		t.Fatal(again)
	}
	// Without asking, an agent's save makes nothing public.
	quiet := call(h, 3, "create_artifact", fmt.Sprintf(`{"title":"Kit 2","kind":"file","content":%q}`, m.URL))
	if quiet["share_url"] != nil {
		t.Fatal(quiet)
	}
	if r := request(h, "GET", fmt.Sprintf("/api/artifacts/%s/shares", quiet["id"]), ""); strings.Contains(r.Body.String(), "token") {
		t.Fatal(r.Body.String())
	}
	// A page shares too, with no file link.
	page := call(h, 4, "create_artifact", `{"title":"Page","kind":"html","content":"<h1>hi</h1>","share":true}`)
	if page["share_url"] == nil || page["download_url"] != nil {
		t.Fatal(page)
	}

	// With sharing off the artifact is still saved, and the agent is told why there is no link.
	off := testServer(t)
	m = upload(t, off, "application/zip", "kit.zip", []byte("kit"))
	res := call(off, 5, "create_artifact", fmt.Sprintf(`{"title":"Kit","kind":"file","content":%q,"share":true}`, m.URL))
	if res["id"] == "" || res["share_url"] != nil || !strings.Contains(res["share_error"].(string), "SHARTIFACTS_SHARE_ORIGIN") {
		t.Fatal(res)
	}
}
