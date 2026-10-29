package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/LaneBirmingham/shartifacts/internal/store"
)

// TestWorkspaceInstallsAsAnApp holds the manifest that makes a phone's "Install" mint an app
// rather than a shortcut: installed without one, the workspace opens inside a browser window
// with a toolbar and an address bar over it. So the pages link it, it parses, it is served as
// JSON, it asks for a window of its own, and it names the 192px and 512px icons Chrome
// requires, plus a maskable one for Android's launcher, which are really there.
func TestWorkspaceInstallsAsAnApp(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	s := &Server{Store: st, Web: os.DirFS("../../web"), AllowedHosts: []string{"shartifacts.example.com"}, PublicOrigin: "https://shartifacts.example.com", ShareOrigin: "https://share.example.com"}
	h := s.Handler()

	for path, page := range map[string]string{"/artifacts": "library", "/": "apps hub"} {
		w := request(h, "GET", path, "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), `<link rel="manifest" href="/manifest.webmanifest"`) {
			t.Fatalf("the %s page does not offer the manifest: %d %s", page, w.Code, w.Body)
		}
	}

	w := request(h, "GET", "/manifest.webmanifest", "")
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/manifest+json" {
		t.Fatalf("manifest: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	if w.Header().Get("Cache-Control") != "no-cache" || w.Header().Get("ETag") == "" {
		t.Fatal("the manifest is cached without revalidating, so an installed app keeps the old one")
	}
	var m struct {
		Name      string `json:"name"`
		ShortName string `json:"short_name"`
		StartURL  string `json:"start_url"`
		Display   string `json:"display"`
		Icons     []struct {
			Src     string `json:"src"`
			Sizes   string `json:"sizes"`
			Type    string `json:"type"`
			Purpose string `json:"purpose"`
		} `json:"icons"`
	}
	if e := json.Unmarshal(w.Body.Bytes(), &m); e != nil {
		t.Fatalf("manifest does not parse: %v", e)
	}
	if m.Name == "" || m.ShortName == "" || m.StartURL == "" || m.Display != "standalone" {
		t.Fatalf("manifest is not installable: %+v", m)
	}
	sizes, purposes := map[string]bool{}, map[string]bool{}
	for _, i := range m.Icons {
		sizes[i.Sizes] = true
		purposes[i.Purpose] = true
		if !strings.HasPrefix(i.Src, "/") || i.Type != "image/png" {
			t.Fatalf("icon %q is not a served PNG", i.Src)
		}
		icon := request(h, "GET", i.Src, "")
		if icon.Code != 200 || icon.Header().Get("Content-Type") != "image/png" || icon.Body.Len() < 200 {
			t.Fatalf("icon %s: %d %s", i.Src, icon.Code, icon.Header().Get("Content-Type"))
		}
	}
	if !sizes["192x192"] || !sizes["512x512"] || !purposes["maskable"] {
		t.Fatalf("manifest needs 192px, 512px and maskable icons, has %v and %v", sizes, purposes)
	}

	// A share hostname is a link to one artifact, not the workspace, so it offers no app to
	// install: only the workspace's own name serves the manifest and its icons.
	share := func(path string) int {
		r := httptest.NewRequest("GET", "https://share.example.com"+path, nil)
		r.RemoteAddr = "203.0.113.7:12345"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	for _, path := range []string{"/manifest.webmanifest", "/icon-192.png", "/icon-512.png", "/icon-maskable-512.png"} {
		if code := share(path); code == 200 {
			t.Fatalf("the share hostname serves %s; it serves only shared artifacts", path)
		}
	}
}

// pngOf encodes a w×h PNG for the launcher icon tests.
func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = byte(i)
	}
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

type installManifest struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	StartURL  string `json:"start_url"`
	Scope     string `json:"scope"`
	Display   string `json:"display"`
	ShortName string `json:"short_name"`
	Icons     []struct {
		Src, Sizes, Type, Purpose string
	} `json:"icons"`
}

// TestAppInstallsAsItsOwnApp holds what lets one app go on a phone's home screen as an app
// of its own, beside the workspace: a manifest made from the app as it is now, with an id and
// scope of its own and a start URL that shows the app alone, and icons that are, in order,
// the ones the app ships, the tile the hub drew, or the workspace's. Nothing about it is
// per app on the server, so a new app installs with no change there.
func TestAppInstallsAsItsOwnApp(t *testing.T) {
	s, _ := appServer(t, nil)
	s.Web = os.DirFS("../../web")
	h := s.Handler()
	w := call(h, "POST", "/api/apps", `{"name":"Broadsheet","icon":"📰","files":{"index.html":"<!doctype html><p>news"}}`)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	id := decodeBody(t, w)["app"].(map[string]any)["id"].(string)
	base := "/apps/" + id

	manifest := func() (installManifest, *httptest.ResponseRecorder) {
		t.Helper()
		w := call(h, "GET", base+"/manifest.webmanifest", "")
		if w.Code != 200 || w.Header().Get("Content-Type") != "application/manifest+json" || w.Header().Get("Cache-Control") != "no-cache" || w.Header().Get("ETag") == "" {
			t.Fatalf("manifest: %d %v %s", w.Code, w.Header(), w.Body)
		}
		var m installManifest
		if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
			t.Fatalf("manifest does not parse: %v", err)
		}
		return m, w
	}
	icon := func(m installManifest, purpose string) []byte {
		t.Helper()
		for _, i := range m.Icons {
			if i.Purpose == purpose {
				w := call(h, "GET", i.Src, "")
				if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || i.Type != "image/png" {
					t.Fatalf("icon %s: %d %s", i.Src, w.Code, w.Header().Get("Content-Type"))
				}
				return w.Body.Bytes()
			}
		}
		return nil
	}

	m, first := manifest()
	if m.ID != base || m.Name != "Broadsheet" || m.ShortName != "Broadsheet" || m.StartURL != base+"?installed" || m.Scope != base || m.Display != "standalone" {
		t.Fatalf("manifest does not install the app on its own: %+v", m)
	}
	if w := call(h, "GET", base+"/manifest.webmanifest", "", "If-None-Match", first.Header().Get("ETag")); w.Code != 304 {
		t.Fatalf("an unchanged manifest should revalidate to 304: %d", w.Code)
	}
	// Before the hub draws a tile, the workspace's icons stand in.
	workspace, _ := os.ReadFile("../../web/icon-512.png")
	if got := icon(m, "any"); !bytes.Equal(got, workspace) || icon(m, "maskable") == nil {
		t.Fatalf("an app with no icon yet should show the workspace's: %d bytes, %+v", len(got), m.Icons)
	}

	// The hub's drawn tile: only a square PNG big enough to install, and named for what it
	// was drawn from.
	put := func(target string, body []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PUT", "http://127.0.0.1:8080"+target, bytes.NewReader(body))
		r.RemoteAddr = "192.168.1.9:12345"
		r.Header.Set("Content-Type", "image/png")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	tile := pngOf(t, 512, 512)
	for name, c := range map[string]struct {
		target string
		body   []byte
		code   int
	}{
		"not a PNG":    {"/api/apps/" + id + "/launcher-icon?for=emoji:x", []byte("<svg/>"), 400},
		"not square":   {"/api/apps/" + id + "/launcher-icon?for=emoji:x", pngOf(t, 512, 256), 400},
		"too small":    {"/api/apps/" + id + "/launcher-icon?for=emoji:x", pngOf(t, 96, 96), 400},
		"truncated":    {"/api/apps/" + id + "/launcher-icon?for=emoji:x", tile[:len(tile)/2], 400},
		"no for":       {"/api/apps/" + id + "/launcher-icon", tile, 400},
		"no such app":  {"/api/apps/000000000000000000000000/launcher-icon?for=emoji:x", tile, 404},
		"a drawn tile": {"/api/apps/" + id + "/launcher-icon?for=emoji:📰", tile, 200},
	} {
		if w := put(c.target, c.body); w.Code != c.code {
			t.Fatalf("%s: %d, want %d: %s", name, w.Code, c.code, w.Body)
		}
	}
	app := decodeBody(t, call(h, "GET", "/api/apps/"+id, ""))
	if app["launcher_icon"] != "emoji:📰" || app["updated_at"] != decodeBody(t, w)["app"].(map[string]any)["updated_at"] {
		t.Fatalf("the app should name its drawn tile and keep its place in the list: %v", app)
	}
	m, _ = manifest()
	if !bytes.Equal(icon(m, "any"), tile) || !bytes.Equal(icon(m, "maskable"), tile) {
		t.Fatalf("the drawn tile should serve as both icons: %+v", m.Icons)
	}
	drawn := m.Icons[0].Src

	// An icon the app ships wins, and a new one is a new URL, so a phone notices. Shipping
	// no maskable one leaves that out rather than mixing in the drawn tile.
	shipped := pngOf(t, 256, 256)
	body, _ := json.Marshal(map[string]any{"base_version": 1, "merge": true, "binary_files": map[string]string{"public/icon.png": base64.StdEncoding.EncodeToString(shipped)}})
	if w := call(h, "POST", "/api/apps/"+id+"/deployments", string(body)); w.Code != 201 {
		t.Fatalf("deploy with an icon: %d %s", w.Code, w.Body)
	}
	m, _ = manifest()
	if len(m.Icons) != 1 || m.Icons[0].Sizes != "256x256" || m.Icons[0].Src == drawn || !bytes.Equal(icon(m, "any"), shipped) {
		t.Fatalf("the shipped icon should replace the tile: %+v", m.Icons)
	}
	if w := call(h, "GET", base+"/icon-maskable.png", ""); w.Code != 404 {
		t.Fatalf("no maskable icon was shipped, yet one is served: %d", w.Code)
	}
	if w := call(h, "GET", "/apps/000000000000000000000000/manifest.webmanifest", ""); w.Code != 404 {
		t.Fatalf("a manifest for no app: %d", w.Code)
	}

	// The hub page itself stays the hub, and the share hostname serves none of this.
	if w := call(h, "GET", base+"?installed", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `src="/hub.js"`) {
		t.Fatalf("the installed start URL should open the hub: %d", w.Code)
	}
	for _, p := range []string{base + "/manifest.webmanifest", base + "/icon.png"} {
		r := httptest.NewRequest("GET", "https://share.example.com"+p, nil)
		r.RemoteAddr = "203.0.113.7:12345"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code == 200 {
			t.Fatalf("the share hostname serves %s", p)
		}
	}
}

// TestAppInstallNeedsSignIn holds that an installed app is the signed-in workspace, not a
// public link: once a password is set, its manifest, icons and the icon upload all refuse a
// browser without a session, and work with one.
func TestAppInstallNeedsSignIn(t *testing.T) {
	_, h := signInServer(t)
	w := call(h, "POST", "/api/apps", `{"name":"Notes","files":{"index.html":"<p>hi"}}`)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	id := decodeBody(t, w)["app"].(map[string]any)["id"].(string)
	cookie := cookieOf(t, call(h, "PUT", "/api/auth/password", `{"password":"correct horse"}`))
	tile := pngOf(t, 192, 192)
	upload := func(headers ...string) int {
		r := httptest.NewRequest("PUT", "http://127.0.0.1:8080/api/apps/"+id+"/launcher-icon?for=letter:N", bytes.NewReader(tile))
		r.RemoteAddr = "192.168.1.9:12345"
		for i := 0; i+1 < len(headers); i += 2 {
			r.Header.Set(headers[i], headers[i+1])
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	if code := upload(); code != 401 {
		t.Fatalf("icon upload without a session: %d", code)
	}
	if code := upload("Cookie", cookie); code != 200 {
		t.Fatalf("icon upload with a session: %d", code)
	}
	for _, p := range []string{"/apps/" + id + "/manifest.webmanifest", "/apps/" + id + "/icon.png"} {
		if w := call(h, "GET", p, ""); w.Code == 200 {
			t.Fatalf("%s without a session: %d", p, w.Code)
		}
		if w := call(h, "GET", p, "", "Cookie", cookie); w.Code != 200 {
			t.Fatalf("%s with a session: %d %s", p, w.Code, w.Body)
		}
	}
}

// TestAppPageLinksItsOwnManifest holds that a full load of an app's hub page already links
// that app's manifest, so a browser judges the page by the app and not by the workspace it
// may have installed. The home page and an unknown app keep the workspace manifest.
func TestAppPageLinksItsOwnManifest(t *testing.T) {
	s, _ := appServer(t, nil)
	s.Web = os.DirFS("../../web")
	h := s.Handler()
	w := call(h, "POST", "/api/apps", `{"name":"Broadsheet","files":{"index.html":"<!doctype html><p>news"}}`)
	id := decodeBody(t, w)["app"].(map[string]any)["id"].(string)

	page := call(h, "GET", "/apps/"+id, "").Body.String()
	for _, want := range []string{`href="/apps/` + id + `/manifest.webmanifest" crossorigin="use-credentials"`, `rel="apple-touch-icon" href="/apps/` + id + `/icon.png"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("the app's page does not link %q", want)
		}
	}
	if strings.Contains(page, `href="/manifest.webmanifest"`) {
		t.Fatal("the app's page still links the workspace manifest")
	}
	for _, p := range []string{"/", "/apps", "/apps/" + strings.Repeat("0", 24), "/apps/not-an-id"} {
		if body := call(h, "GET", p, "").Body.String(); !strings.Contains(body, `href="/manifest.webmanifest"`) {
			t.Fatalf("%s should keep the workspace manifest", p)
		}
	}
}
