package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image/png"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/LaneBirmingham/shartifacts/internal/store"
)

// Installing an app as an app of its own. The workspace installs as one app
// (web/manifest.webmanifest); each app can install on its own too, with its own name and
// icon on the phone's home screen. The hub's page for an app, /apps/{id}, links a manifest
// made for that app, whose scope is that page and whose start_url adds ?installed, which
// tells the hub to show the app alone, with none of the workspace around it.
//
// Everything in the manifest comes from what the app is now (its name, its live version's
// icon), so an app installs as soon as it exists, with no change to the server, and an
// installed app picks up a new name or icon the next time the phone checks its manifest.
//
// The manifest and icons are workspace pages like any other: they need sign-in when it is
// on (the hub asks for the manifest with credentials), and the share and app hostnames
// never serve them. An installed app is a window onto the signed-in workspace, not a
// public link.
//
// The icon is, first, one the app ships: icon.png (and icon-maskable.png, with its picture
// inside the middle 80% for launchers that crop) at the live version's web root, square
// and at least 192px. Without one, it is a tile the hub draws from the app's emoji or the
// first letter of its name, since a browser has the emoji font and this server does not;
// before the hub has drawn one, the workspace's own icon.

// launcherIconMax bounds an icon the hub uploads or an app ships.
const launcherIconMax = 1 << 20

func (s *Server) appInstallRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /apps/{id}/manifest.webmanifest", s.appManifest)
	mux.HandleFunc("GET /apps/{id}/icon.png", func(w http.ResponseWriter, r *http.Request) { s.serveLauncherIcon(w, r, false) })
	mux.HandleFunc("GET /apps/{id}/icon-maskable.png", func(w http.ResponseWriter, r *http.Request) { s.serveLauncherIcon(w, r, true) })
	mux.HandleFunc("PUT /api/apps/{id}/launcher-icon", func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(io.LimitReader(r.Body, launcherIconMax+1))
		if err != nil {
			respond(w, 400, nil, err)
			return
		}
		if _, ok := launcherPNG(b); !ok {
			respond(w, 400, nil, errors.New("the body must be a square PNG of 192–2048 pixels, up to 1 MiB"))
			return
		}
		// A decodable header is not a decodable image; the phone gets only whole ones.
		if _, err := png.Decode(bytes.NewReader(b)); err != nil {
			respond(w, 400, nil, errors.New("the PNG does not decode: "+err.Error()))
			return
		}
		a, err := s.Store.SetLauncherIcon(r.PathValue("id"), r.URL.Query().Get("for"), b)
		respond(w, 200, map[string]string{"launcher_icon": a.LauncherIcon}, err)
	})
}

// launcherPNG reports a PNG's width when it is square and big enough for a launcher:
// Chrome wants at least 192px to install, and anything past 2048px is only weight.
func launcherPNG(b []byte) (int, bool) {
	if len(b) == 0 || len(b) > launcherIconMax {
		return 0, false
	}
	c, err := png.DecodeConfig(bytes.NewReader(b))
	if err != nil || c.Width != c.Height || c.Width < 192 || c.Width > 2048 {
		return 0, false
	}
	return c.Width, true
}

// shippedIcon reads a launcher icon from the app's live version, or nil.
func (s *Server) shippedIcon(a store.App, name string) []byte {
	if a.Live == 0 {
		return nil
	}
	site := s.site(a.ID, a.Live)
	if !site.Frontend {
		return nil
	}
	f, ok := s.frontendFile(a.ID, a.Live, site, name)
	if !ok || f == "dir" {
		return nil
	}
	b, err := os.ReadFile(f)
	if _, fit := launcherPNG(b); err != nil || !fit {
		return nil
	}
	return b
}

// launcherIcons are the app's icons for launchers that show it whole and those that crop
// it to a shape; maskable is nil when the app ships an icon of its own but no maskable one,
// rather than a drawn tile that would look like another app.
func (s *Server) launcherIcons(a store.App) (whole, maskable []byte) {
	if b := s.shippedIcon(a, "icon.png"); b != nil {
		return b, s.shippedIcon(a, "icon-maskable.png")
	}
	if b, err := s.Store.LauncherIcon(a.ID); err == nil {
		if _, ok := launcherPNG(b); ok {
			// Drawn with its picture in the middle, the tile suits both.
			return b, b
		}
	}
	whole, _ = fs.ReadFile(s.Web, "icon-512.png")
	maskable, _ = fs.ReadFile(s.Web, "icon-maskable-512.png")
	return whole, maskable
}

type manifestIcon struct {
	Src     string `json:"src"`
	Sizes   string `json:"sizes"`
	Type    string `json:"type"`
	Purpose string `json:"purpose"`
}

func (s *Server) appManifest(w http.ResponseWriter, r *http.Request) {
	a, err := s.Store.App(r.PathValue("id"))
	if err != nil {
		respond(w, 404, nil, err)
		return
	}
	base := "/apps/" + a.ID
	whole, maskable := s.launcherIcons(a)
	icons := []manifestIcon{}
	for _, icon := range []struct {
		b       []byte
		file    string
		purpose string
	}{{whole, "icon.png", "any"}, {maskable, "icon-maskable.png", "maskable"}} {
		// The icon's hash in its URL makes a new icon a new URL, which is how a phone
		// notices that an installed app's icon changed.
		if n, ok := launcherPNG(icon.b); ok {
			size := strconv.Itoa(n)
			icons = append(icons, manifestIcon{Src: base + "/" + icon.file + "?v=" + shortHash(icon.b), Sizes: size + "x" + size, Type: "image/png", Purpose: icon.purpose})
		}
	}
	// The id keeps each app apart from the workspace (id "/") and from each other, so all
	// of them install side by side. The colours are the workspace's paper, which the
	// drawn tiles share.
	b, _ := json.Marshal(struct {
		ID          string         `json:"id"`
		Name        string         `json:"name"`
		ShortName   string         `json:"short_name"`
		Description string         `json:"description,omitempty"`
		StartURL    string         `json:"start_url"`
		Scope       string         `json:"scope"`
		Display     string         `json:"display"`
		Background  string         `json:"background_color"`
		Theme       string         `json:"theme_color"`
		Icons       []manifestIcon `json:"icons"`
	}{base, a.Name, a.Name, a.Description, base + "?installed", base, "standalone", "#f9f8f6", "#f9f8f6", icons})
	serveRevalidated(w, r, "application/manifest+json", b)
}

// appPageID returns the app id of a hub page /apps/{id}.
func appPageID(p string) (string, bool) {
	id, ok := strings.CutPrefix(p, "/apps/")
	if !ok || len(id) != 24 {
		return "", false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return "", false
		}
	}
	return id, true
}

// writeHubAppPage serves the hub with this app's manifest already linked. A browser decides
// whether a page installs, and as what, from the manifest it finds as the page loads; the
// hub swapping the home manifest for the app's afterwards (offerInstall) can leave Chrome
// judging the page by the workspace's, which may already be installed. An unknown id gets
// the plain hub, which says so itself.
func (s *Server) writeHubAppPage(w http.ResponseWriter, id string) {
	b, err := fs.ReadFile(s.Web, "hub.html")
	if err != nil {
		http.Error(w, "UI unavailable", 500)
		return
	}
	if _, err := s.Store.App(id); err == nil {
		base := "/apps/" + id
		b = bytes.Replace(b, []byte(`<link rel="manifest" href="/manifest.webmanifest" />`), []byte(`<link rel="manifest" href="`+base+`/manifest.webmanifest" crossorigin="use-credentials" />`), 1)
		b = bytes.Replace(b, []byte(`<link rel="apple-touch-icon" href="/icon-192.png" />`), []byte(`<link rel="apple-touch-icon" href="`+base+`/icon.png" />`), 1)
	}
	writeHTML(w, b)
}

func (s *Server) serveLauncherIcon(w http.ResponseWriter, r *http.Request, mask bool) {
	a, err := s.Store.App(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	b, maskable := s.launcherIcons(a)
	if mask {
		b = maskable
	}
	if b == nil {
		http.NotFound(w, r)
		return
	}
	serveRevalidated(w, r, "image/png", b)
}

// serveRevalidated serves b with an ETag and no-cache: an installed app should see a new
// name or icon, and an unchanged one costs a 304.
func serveRevalidated(w http.ResponseWriter, r *http.Request, contentType string, b []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", `"`+shortHash(b)+`"`)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b))
}

func shortHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}
