package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LaneBirmingham/shartifacts/internal/store"
)

func TestMultiFileArtifacts(t *testing.T) {
	h := testServer(t)
	body, _ := json.Marshal(map[string]any{"title": "Site", "kind": "html", "content": `<link rel="stylesheet" href="style.css"><a href="about.html">About</a><script type="module" src="lib/app.js"></script>`,
		"files": map[string]string{"style.css": "h1{color:red}", "lib/app.js": "import {n} from './n.js'; document.title = n", "lib/n.js": "export const n = 'ok'", "about.html": "<h1>About</h1>"}})
	w := request(h, "POST", "/api/artifacts", string(body))
	var a store.Artifact
	json.Unmarshal(w.Body.Bytes(), &a)
	if w.Code != 201 || len(a.Revisions[0].Files) != 4 {
		t.Fatal(w.Code, w.Body.String())
	}
	// The classic preview URL moves into the version's directory.
	if w = request(h, "GET", "/preview/"+a.ID+"?version=1", ""); w.Code != 302 || w.Header().Get("Location") != "/preview/"+a.ID+"/1/" {
		t.Fatal(w.Code, w.Header())
	}
	w = request(h, "GET", "/preview/"+a.ID+"/1/", "")
	csp := w.Header().Get("Content-Security-Policy")
	own := "http://127.0.0.1:8080/preview/" + a.ID + "/1/"
	if w.Code != 200 || !strings.Contains(w.Body.String(), `href="style.css"`) || !strings.Contains(csp, "script-src 'unsafe-inline' 'unsafe-eval' http://127.0.0.1:8080/vendor/ http://127.0.0.1:8080/runtime.js "+own) || !strings.Contains(csp, "connect-src "+own+" http://127.0.0.1:8080/_blob/;") {
		t.Fatal(w.Code, csp)
	}
	// Files are served raw with their type; other pages are sandboxed previews too.
	sandboxed := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "http://127.0.0.1:8080"+path, nil)
		r.RemoteAddr = "192.168.1.9:1"
		r.Header.Set("Origin", "null")
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w = sandboxed("/preview/" + a.ID + "/1/lib/app.js")
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/javascript") || w.Header().Get("Access-Control-Allow-Origin") != "*" || w.Header().Get("Content-Security-Policy") != "default-src 'none'; sandbox" {
		t.Fatal(w.Code, w.Header())
	}
	if w = sandboxed("/preview/" + a.ID + "/1/about.html"); w.Code != 200 || !strings.Contains(w.Body.String(), "<h1>About</h1>") || !strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox allow-scripts") {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, path := range []string{"/preview/" + a.ID + "/1/missing.css", "/preview/" + a.ID + "/2/", "/preview/" + a.ID + "/1/../x"} {
		// guard refuses "../", so a path can never leave the directory.
		if w = sandboxed(path); w.Code != 404 && w.Code != 400 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	// The guard still refuses sandboxed writes.
	r := httptest.NewRequest("POST", "http://127.0.0.1:8080/preview/"+a.ID+"/1/", nil)
	r.RemoteAddr = "192.168.1.9:1"
	r.Header.Set("Origin", "null")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 403 {
		t.Fatal("cross-origin POST allowed", rec.Code)
	}
	// Download is a zip of the page and its files.
	w = request(h, "GET", "/api/artifacts/"+a.ID+"/download", "")
	zr, e := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if e != nil || w.Header().Get("Content-Type") != "application/zip" {
		t.Fatal(e, w.Header())
	}
	names := []string{}
	for _, f := range zr.File {
		names = append(names, f.Name)
		if f.Name == "style.css" {
			rc, _ := f.Open()
			b, _ := io.ReadAll(rc)
			if string(b) != "h1{color:red}" {
				t.Fatal(string(b))
			}
		}
	}
	if strings.Join(names, ",") != "index.html,about.html,lib/app.js,lib/n.js,style.css" {
		t.Fatal(names)
	}
	// Drafts with files are directories too, readable until they expire.
	w = request(h, "POST", "/api/previews", `{"title":"d","kind":"html","content":"<link rel=stylesheet href=s.css>","files":{"s.css":"p{}"}}`)
	var d map[string]string
	json.Unmarshal(w.Body.Bytes(), &d)
	if !strings.HasSuffix(d["url"], "/") {
		t.Fatal(d)
	}
	for i := 0; i < 2; i++ {
		if w = sandboxed(d["url"] + "s.css"); w.Code != 200 || w.Body.String() != "p{}" {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w = request(h, "POST", "/api/previews", `{"title":"d","kind":"html","content":"x","files":{"../s.css":"p{}"}}`); w.Code != 400 {
		t.Fatal("bad draft path accepted")
	}
}

func TestReactImportsItsFiles(t *testing.T) {
	files := map[string]string{
		"components/Counter.tsx": `import {useState} from 'react'; import {label} from '../lib/text'; import './counter.css'; export default function Counter(){const [n,set]=useState(0);return <button onClick={()=>set(n+1)}>{label} {n}</button>}`,
		"components/counter.css": `button{color:rebeccapurple}`,
		"lib/text.ts":            `import data from '../data.json'; export const label: string = data.label`,
		"data.json":              `{"label":"Clicks"}`,
	}
	code, css, _, e := compileReact(`import Counter from './components/Counter'; export default () => <Counter />`, files)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(code, "Clicks") || !strings.Contains(css, "rebeccapurple") {
		t.Fatal(css)
	}
	if _, _, _, e = compileReact(`import x from './missing'; export default x`, files); e == nil || !strings.Contains(e.Error(), "not among the artifact's files") {
		t.Fatal(e)
	}
	if _, _, _, e = compileReact(`import x from '../../etc/passwd'; export default x`, files); e == nil {
		t.Fatal("escaped the files")
	}
	broken := map[string]string{"Bad.tsx": "export default function( {"}
	if _, _, _, e = compileReact(`import Bad from './Bad'; export default Bad`, broken); e == nil || !strings.Contains(e.Error(), "Bad.tsx") {
		t.Fatal("error does not name the file:", e)
	}
}
