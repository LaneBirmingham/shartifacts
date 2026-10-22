package server

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LaneBirmingham/shartifacts/internal/store"
)

func TestMediaHTTPRangeAndSandboxAccess(t *testing.T) {
	h := testServer(t)
	send := func(method, path, typ, origin, site, rangeHeader string, body []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1:8080"+path, bytes.NewReader(body))
		r.RemoteAddr = "192.168.0.2:1234"
		r.Header.Set("Content-Type", typ)
		r.Header.Set("Origin", origin)
		r.Header.Set("Sec-Fetch-Site", site)
		r.Header.Set("Range", rangeHeader)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	data := []byte("0123456789media")
	w := send("POST", "/api/media", "video/mp4", "", "", "", data)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	var media store.Media
	if err := json.Unmarshal(w.Body.Bytes(), &media); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"GET", "HEAD"} {
		w = send(method, media.URL, "", "null", "cross-site", "bytes=2-5", nil)
		if w.Code != 206 || w.Header().Get("Content-Type") != "video/mp4" || w.Header().Get("Content-Range") != "bytes 2-5/15" || w.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Fatal(w.Code, w.Header(), w.Body.String())
		}
		if method == "GET" && w.Body.String() != "2345" {
			t.Fatal("incorrect byte range")
		}
	}
	for _, path := range []string{"/api/artifacts", "/api/media", "/mcp"} {
		w = send("POST", path, "video/mp4", "null", "cross-site", "", data)
		if w.Code != 403 {
			t.Fatal("sandbox write allowed", path, w.Code)
		}
	}
	w = send("POST", "/api/media", "text/html", "", "", "", data)
	if w.Code != 400 {
		t.Fatal("active content allowed", w.Code)
	}
	w = send("GET", media.URL, "", "", "", "bytes=999-1000", nil)
	if w.Code != 416 {
		t.Fatal("invalid range", w.Code)
	}
	w = request(h, "GET", "/api/media", "")
	if !strings.Contains(w.Body.String(), media.URL) {
		t.Fatal(w.Body.String())
	}
	w = request(h, "POST", "/api/artifacts", `{"title":"media","kind":"html","content":"<video controls></video>"}`)
	var a store.Artifact
	json.Unmarshal(w.Body.Bytes(), &a)
	w = request(h, "GET", "/preview/"+a.ID, "")
	csp := w.Header().Get("Content-Security-Policy")
	for _, part := range []string{"media-src data: blob: http://127.0.0.1:8080/media/", "img-src data: blob: http://127.0.0.1:8080/media/", "connect-src http://127.0.0.1:8080/_blob/;", "frame-src 'none'", "sandbox allow-scripts"} {
		if !strings.Contains(csp, part) {
			t.Fatal(csp, part)
		}
	}
}

func TestDataFilesKeepNames(t *testing.T) {
	h := testServer(t)
	r := httptest.NewRequest("POST", "http://127.0.0.1:8080/api/media?name=C:%5Cdata%5Csales.csv", strings.NewReader("region,total\nnorth,5\n"))
	r.RemoteAddr = "192.168.0.2:1"
	r.Header.Set("Content-Type", "text/csv")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var m store.Media
	json.Unmarshal(w.Body.Bytes(), &m)
	if w.Code != 201 || m.Name != "sales.csv" || !strings.HasSuffix(m.URL, ".csv") {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = request(h, "GET", "/api/media", ""); !strings.Contains(w.Body.String(), `"name":"sales.csv"`) {
		t.Fatal(w.Body.String())
	}
	w = request(h, "GET", m.URL, "")
	if w.Header().Get("Content-Type") != "text/csv" || w.Header().Get("Content-Security-Policy") != "default-src 'none'; sandbox" {
		t.Fatal(w.Header())
	}
}
