package server

import (
	"encoding/json"
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
