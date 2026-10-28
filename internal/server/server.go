package server

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LaneBirmingham/shartifacts/internal/auth"
	"github.com/LaneBirmingham/shartifacts/internal/runner"
	"github.com/LaneBirmingham/shartifacts/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Server struct {
	Store        *store.Store
	Web          fs.FS
	AllowedHosts []string
	// PublicOrigin is the operator-declared external origin of a TLS-terminating proxy in
	// front of this app (e.g. https://shartifacts.example.com). The proxy forwards plain
	// HTTP, so the app cannot learn the external scheme from the request and forwarded
	// headers are deliberately ignored; the scheme is configuration instead. It applies to
	// that host only. Empty = the app is reached directly, as before. When set it is also
	// the host of the links MCP tools hand to people (linkBase).
	PublicOrigin string
	// ShareOrigin is the external origin of read-only share links (e.g.
	// https://share.example.com). On its hostname only share pages are served. Empty
	// turns sharing outside the network off.
	ShareOrigin string
	// AppOrigin gives each app's public links an origin of the app's own, one hostname label
	// per app (e.g. https://*.share.example.com), so their pages keep real localStorage,
	// IndexedDB and cookies, apart from every other app's. Empty serves app links on
	// ShareOrigin, in the opaque-origin sandbox.
	AppOrigin string
	// AuthProxy accepts workspace requests a proxy forwards from public addresses: the
	// operator says that proxy authenticates people. Otherwise they are refused (forwarded.go).
	AuthProxy bool
	// Auth is the workspace's own sign-in (auth.go), on once an owner password is set. Nil
	// opens the data directory's.
	Auth *auth.Store
	// NoChunkUpload hides begin_media_upload/upload_media_chunk (shell agents use the curl upload).
	NoChunkUpload bool
	// Claude answers window.claude.complete and the sample capability; nil leaves it off.
	Claude *Claude
	// Connectors are the MCP servers the mcp capability reaches.
	Connectors []Connector
	// Runtime builds and runs app backends: a runner sidecar (runner.Remote), or nil to run
	// them beside this process.
	Runtime       runner.Runtime
	attempts      passwordAttempts
	grantKeyOnce  sync.Once
	grantKeyBytes []byte
	appKeyOnce    sync.Once
	appSessionKey []byte
	appLabelOnce  sync.Once
	appLabelKey   []byte
	appSites      sync.Map
	appBuilds     sync.Map
	appErrors     appErrorLog
	connectors    connectorHub
	draftMu       sync.Mutex
	drafts        map[string]draft
	etags         sync.Map
	errors        runtimeErrors
	hub           *hub
	hubOnce       sync.Once
	handler       http.Handler
	handlerOnce   sync.Once
	search        searchCache
}
type draft struct {
	Revision store.Revision
	Expires  time.Time
}

// Handler serves everything: the workspace on its own names, and the share hostname and
// app hostnames by the Host a proxy forwards.
func (s *Server) Handler() http.Handler {
	s.handlerOnce.Do(func() { s.handler = s.build() })
	return s.handler
}

type publicListenerKey struct{}

// PublicHandler is for the listener the edge proxy forwards to (-share-listen). It never
// serves the workspace: a request for an app's hostname goes to that app, and any other is
// answered as the share hostname, whatever Host the proxy sent. On the main listener a proxy
// that rewrites Host to its upstream address (nginx's default) would reach the workspace.
// Its content is public by design, so it answers any peer.
func (s *Server) PublicHandler() http.Handler {
	h := s.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, app := s.appHost(r); !app {
			r.Host = s.shareHost()
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), publicListenerKey{}, true)))
	})
}

func (s *Server) build() http.Handler {
	s.hubOnce.Do(func() {
		s.hub = newHub()
		s.hub.visible = func(artifact, path string) bool {
			return s.Store.DocReadable(artifact, path, store.Viewer{ReadOnly: true})
		}
		s.Store.OnChange(s.hub.changed)
		if s.Runtime == nil {
			s.Runtime = &runner.Local{Dir: s.Store.AppsDir()}
		}
		if s.Auth == nil {
			a, err := auth.Open(s.Store.Dir())
			if err != nil {
				panic(fmt.Sprintf("sign-in state: %v", err))
			}
			s.Auth = a
		}
	})
	mux := http.NewServeMux()
	s.connectors.list = s.Connectors
	s.capabilityRoutes(mux)
	s.connectorRoutes(mux)
	s.shareRoutes(mux)
	s.searchRoutes(mux)
	s.appRoutes(mux)
	s.authRoutes(mux)
	mux.HandleFunc("GET /api/media", func(w http.ResponseWriter, r *http.Request) {
		items, e := s.Store.ListMedia()
		if e != nil || r.URL.Query().Get("usage") == "" {
			respond(w, 200, map[string]any{"media": items}, e)
			return
		}
		// ?usage=1 also says which artifacts and apps name each file: the media dialog's
		// unused-file finder, which reads every artifact, so only on request.
		users, e := s.Store.MediaUsage(items)
		type withUsers struct {
			store.Media
			UsedBy []store.MediaUser `json:"used_by"`
		}
		out := make([]withUsers, len(items))
		for i, m := range items {
			out[i] = withUsers{m, users[m.ID]}
			if out[i].UsedBy == nil {
				out[i].UsedBy = []store.MediaUser{}
			}
		}
		respond(w, 200, map[string]any{"media": out}, e)
	})
	mux.HandleFunc("DELETE /api/media/{id}", func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, map[string]bool{"deleted": true}, s.Store.DeleteMedia(r.PathValue("id")))
	})
	mux.HandleFunc("POST /api/media", s.uploadMedia)
	mux.HandleFunc("GET /media/{id}", func(w http.ResponseWriter, r *http.Request) { s.serveMedia(w, r, r.PathValue("id")) })
	mux.HandleFunc("GET /api/artifacts", func(w http.ResponseWriter, r *http.Request) {
		// The library by default; ?archived=only is the archive, ?archived=include both.
		f, e := store.ParseArchiveFilter(r.URL.Query().Get("archived"))
		if e != nil {
			respond(w, 400, nil, e)
			return
		}
		writeJSON(w, 200, s.Store.List(f))
	})
	s.archiveRoutes(mux)
	mux.HandleFunc("POST /api/artifacts", func(w http.ResponseWriter, r *http.Request) {
		var in store.Input
		if !decode(w, r, &in) {
			return
		}
		in.Owner = s.viewerID(r)
		a, e := s.Store.Save("", in)
		respond(w, 201, a, e)
	})
	mux.HandleFunc("GET /api/artifacts/{id}", func(w http.ResponseWriter, r *http.Request) {
		a, e := s.Store.Get(r.PathValue("id"))
		respond(w, 200, a, e)
	})
	mux.HandleFunc("PUT /api/artifacts/{id}", func(w http.ResponseWriter, r *http.Request) {
		var in store.Input
		if !decode(w, r, &in) {
			return
		}
		a, e := s.Store.Save(r.PathValue("id"), in)
		respond(w, 200, a, e)
	})
	mux.HandleFunc("DELETE /api/artifacts/{id}", func(w http.ResponseWriter, r *http.Request) {
		v, _ := strconv.Atoi(r.URL.Query().Get("base_version"))
		e := s.Store.Delete(r.PathValue("id"), v)
		respond(w, 200, map[string]bool{"deleted": true}, e)
	})
	mux.HandleFunc("GET /api/artifacts/{id}/download", s.download)
	mux.HandleFunc("POST /api/artifacts/{id}/storage", s.storage)
	s.commentRoutes(mux)
	mux.HandleFunc("POST /api/complete", s.complete)
	mux.HandleFunc("POST /api/artifacts/{id}/errors", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Version int    `json:"version"`
			Message string `json:"message"`
		}
		if !decode(w, r, &in) {
			return
		}
		respond(w, 202, map[string]bool{"recorded": true}, s.reportError(r.PathValue("id"), in))
	})
	mux.HandleFunc("PUT /api/artifacts/{id}/pin", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Pinned bool `json:"pinned"`
		}
		if !decode(w, r, &in) {
			return
		}
		a, e := s.Store.SetPinned(r.PathValue("id"), in.Pinned)
		respond(w, 200, a, e)
	})
	mux.HandleFunc("GET /preview/{id}", s.preview)
	mux.HandleFunc("GET /preview/{id}/{version}/{path...}", s.previewFile)
	mux.HandleFunc("POST /api/previews", func(w http.ResponseWriter, r *http.Request) {
		var in store.Input
		if !decode(w, r, &in) {
			return
		}
		if len(in.Content) > store.MaxContent {
			writeJSON(w, 400, map[string]string{"error": "content exceeds 2 MiB"})
			return
		}
		files := map[string]string{}
		for p, v := range in.Files {
			if v != nil {
				files[p] = *v
			}
		}
		if e := store.ValidateFiles(in.Content, files); e != nil {
			writeJSON(w, 400, map[string]string{"error": e.Error()})
			return
		}
		b := make([]byte, 16)
		if _, e := rand.Read(b); e != nil {
			http.Error(w, "cannot create preview", 500)
			return
		}
		id := hex.EncodeToString(b)
		s.draftMu.Lock()
		defer s.draftMu.Unlock()
		if s.drafts == nil {
			s.drafts = map[string]draft{}
		}
		for k, v := range s.drafts {
			if time.Now().After(v.Expires) {
				delete(s.drafts, k)
			}
		}
		if len(s.drafts) >= 100 {
			writeJSON(w, 429, map[string]string{"error": "preview capacity reached; saved artifacts still work"})
			return
		}
		s.drafts[id] = draft{store.Revision{Title: in.Title, Kind: in.Kind, Content: in.Content, Files: files}, time.Now().Add(2 * time.Minute)}
		// A draft with files is a directory its page loads from, so it stays until it expires.
		url := "/draft-preview/" + id
		if len(files) > 0 {
			url += "/"
		}
		writeJSON(w, 201, map[string]string{"url": url})
	})
	mux.HandleFunc("GET /draft-preview/{id}/{path...}", func(w http.ResponseWriter, r *http.Request) {
		s.draftMu.Lock()
		d, ok := s.drafts[r.PathValue("id")]
		s.draftMu.Unlock()
		if !ok || time.Now().After(d.Expires) || len(d.Revision.Files) == 0 {
			http.Error(w, "preview expired", 404)
			return
		}
		s.serveArtifactPath(w, r, d.Revision, "/draft-preview/"+r.PathValue("id")+"/", "")
	})
	mux.HandleFunc("GET /draft-preview/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.draftMu.Lock()
		d, ok := s.drafts[r.PathValue("id")]
		delete(s.drafts, r.PathValue("id"))
		s.draftMu.Unlock()
		if !ok || time.Now().After(d.Expires) {
			http.Error(w, "preview expired", 404)
			return
		}
		s.renderPreview(w, r, d.Revision, "", "")
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		scheme := s.clientScheme(r)
		return s.mcpServer(scheme+"://"+r.Host, s.uploadPath(r))
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, MaxRequestBodyBytes: 12 << 20}))
	files := http.FileServer(http.FS(s.Web))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" {
			http.Error(w, "method not allowed", 405)
			return
		}
		if page := workspacePage(r.URL.Path); page != "" {
			s.writePage(w, page)
			return
		}
		// The UI's pages are served only by their routes, with the page CSP.
		if path.Ext(r.URL.Path) == ".html" {
			http.NotFound(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/vendor/") {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			// Libraries reload with every preview; let the browser revalidate instead.
			if tag := s.etag(r.URL.Path); tag != "" {
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("ETag", tag)
			}
		}
		// The manifest's type is not in every platform's mime table, and a phone that
		// installed the app should pick up changes to it, so say both and revalidate.
		if r.URL.Path == "/manifest.webmanifest" {
			w.Header().Set("Content-Type", "application/manifest+json")
			if tag := s.etag(r.URL.Path); tag != "" {
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("ETag", tag)
			}
		}
		files.ServeHTTP(w, r)
	})
	return s.guard(mux)
}

// etag hashes an embedded file once; the web assets never change while running.
func (s *Server) serveMedia(w http.ResponseWriter, r *http.Request, id string) {
	path, t, e := s.Store.MediaPath(id)
	if e != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", t)
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	http.ServeFile(w, r, path)
}

func (s *Server) etag(path string) string {
	if v, ok := s.etags.Load(path); ok {
		return v.(string)
	}
	b, e := fs.ReadFile(s.Web, strings.TrimPrefix(path, "/"))
	if e != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	tag := `"` + hex.EncodeToString(sum[:12]) + `"`
	s.etags.Store(path, tag)
	return tag
}

// dotSegments reports whether a decoded URL path has a "." or ".." segment.
func dotSegments(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

// workspacePage is the page of the UI a path shows, or "".
func workspacePage(p string) string {
	switch {
	case p == "/" || p == "/apps" || strings.HasPrefix(p, "/apps/"):
		return "hub.html"
	case p == "/artifacts" || strings.HasPrefix(p, "/a/"):
		return "index.html"
	case strings.HasPrefix(p, "/view/"):
		return "view.html"
	}
	return ""
}

func (s *Server) writePage(w http.ResponseWriter, page string) {
	b, e := fs.ReadFile(s.Web, page)
	if e != nil {
		http.Error(w, "UI unavailable", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; frame-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
	w.Write(b)
}

func (s *Server) uploadMedia(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, store.MaxMedia+1)
	media, e := s.Store.SaveMedia(r.Header.Get("Content-Type"), r.Body)
	if e == nil && r.URL.Query().Has("name") {
		media, e = s.Store.NameMedia(media, r.URL.Query().Get("name"))
	}
	respond(w, 201, media, e)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
func respond(w http.ResponseWriter, status int, v any, e error) {
	if e != nil {
		status = 400
		if errors.Is(e, store.ErrNotFound) {
			status = 404
		} else if errors.Is(e, store.ErrConflict) {
			status = 409
		}
		writeJSON(w, status, map[string]string{"error": e.Error()})
		return
	}
	writeJSON(w, status, v)
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeJSON(w, 415, map[string]string{"error": "Content-Type must be application/json"})
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 12<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		writeJSON(w, 400, map[string]string{"error": e.Error()})
		return false
	}
	if e := d.Decode(&struct{}{}); e != io.EOF {
		writeJSON(w, 400, map[string]string{"error": "expected one JSON object"})
		return false
	}
	return true
}
func localIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsLoopback() || ip.IsPrivate() || netip.MustParsePrefix("100.64.0.0/10").Contains(ip)
}

// clientScheme reports the scheme the BROWSER used, which is what the Origin check has to
// compare against and what the app advertises in upload URLs and preview CSPs.
//
// Normally that is the scheme of this very connection. It stops being knowable when a
// TLS-terminating proxy fronts the app (tailscale serve, Caddy, nginx): it forwards plain
// HTTP, so r.TLS is nil while the browser's Origin is https:// - every fetch("/api/...")
// from the loaded page would be rejected 403 cross-origin-request-denied, leaving a working
// shell with an empty library.
//
// The scheme therefore comes from configuration, not from the wire: -public-origin declares
// the external origin and is applied to that host only. Forwarded headers stay ignored on
// purpose (see the README's security model) - they are attacker-controlled whenever a client
// can reach the port directly, and honouring them would move the trust boundary into the
// request. With no -public-origin the behaviour is exactly as before.
func (s *Server) clientScheme(r *http.Request) string {
	if s.isShareHost(r) {
		if u, e := url.Parse(s.ShareOrigin); e == nil {
			return u.Scheme
		}
	}
	if _, ok := s.appHost(r); ok {
		scheme, _, _ := strings.Cut(s.AppOrigin, "://")
		return scheme
	}
	if s.PublicOrigin != "" {
		if u, e := url.Parse(s.PublicOrigin); e == nil && strings.EqualFold(u.Host, r.Host) {
			return u.Scheme
		}
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// linkBase is where links meant for people point: -public-origin when it is set, since agents
// usually connect by LAN address (http://192.168.1.20:8080/mcp) while people open the real
// name, else the address the agent connected to. Upload URLs and curl examples stay on the
// connected address: the agent's own shell has to reach those.
func (s *Server) linkBase(connected string) string {
	if s.PublicOrigin != "" {
		return strings.TrimRight(s.PublicOrigin, "/")
	}
	return connected
}

// sandboxReadable paths may be read by opaque-origin previews, whose requests are always
// cross-site: bundled libraries, media, and a multi-file artifact's pages and files.
func sandboxReadable(p string) bool {
	for _, prefix := range []string{"/vendor/", "/media/", "/preview/", "/draft-preview/", "/_blob/", "/runtime.js", "/s/"} {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		host, _, e := net.SplitHostPort(r.RemoteAddr)
		if e != nil {
			http.Error(w, "invalid peer", 403)
			return
		}
		// Browsers resolve dot segments before sending a URL, and the mux redirects the ones
		// written plainly, but encoded ones (%2e%2e) arrive decoded in the path. Each check
		// below reads the path by its prefix, so "/s/../hub.html" must not get that far.
		if dotSegments(r.URL.Path) {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
		ip, e := netip.ParseAddr(host)
		public := r.Context().Value(publicListenerKey{}) != nil
		if !public && (e != nil || !localIP(ip)) {
			http.Error(w, "local network access only", 403)
			return
		}
		// The reverse proxy asks whether to get a certificate for a hostname (tlsAllowed).
		// It calls by its own name for this server, so this precedes the Host check.
		if r.URL.Path == TLSAllowedPath {
			s.tlsAllowed(w, r)
			return
		}
		h := r.Host
		if v, _, err := net.SplitHostPort(h); err == nil {
			h = v
		}
		h = strings.TrimSuffix(strings.ToLower(h), ".")
		allowed := h == "localhost"
		if ip, e := netip.ParseAddr(h); e == nil && localIP(ip) {
			allowed = true
		}
		for _, v := range s.AllowedHosts {
			if strings.EqualFold(h, strings.TrimSpace(v)) {
				allowed = true
			}
		}
		// The share hostname serves share pages and the files they load, nothing else.
		if s.isShareHost(r) {
			if !sharePublic(r) {
				http.NotFound(w, r)
				return
			}
			allowed = true
		}
		// An app's own hostname serves its public links and the libraries they load.
		_, appHost := s.appHost(r)
		if appHost {
			if !appHostPublic(r) {
				http.NotFound(w, r)
				return
			}
			allowed = true
		}
		if !allowed {
			http.Error(w, "host not allowed: use a LAN IP or add this hostname to SHARTIFACTS_ALLOWED_HOSTS (-allowed-hosts)", 403)
			return
		}
		if !appHost && !s.isShareHost(r) && !s.AuthProxy && r.URL.Path != "/healthz" {
			if client, public := forwardedPublic(r); public {
				refuseForwardedPublic(w, r, client)
				return
			}
		}
		if appHost && !s.fromAppItself(r) {
			http.Error(w, "cross-site request denied", 403)
			return
		}
		// App pages and backends decide for themselves (serveApp): their sandboxed pages
		// always call cross-origin, and only with the session the page was served.
		if origin := r.Header.Get("Origin"); origin != "" && !appPath(r.URL.Path) {
			u, e := url.Parse(origin)
			scheme := s.clientScheme(r)
			if e != nil || u.Host != r.Host || u.Scheme != scheme {
				if !((r.Method == "GET" || r.Method == "HEAD") && sandboxReadable(r.URL.Path)) {
					http.Error(w, "cross-origin request denied", 403)
					return
				}
			}
		}
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" && !appPath(r.URL.Path) {
			// Following a link into the app (chat apps, bookmarks, redirects) is a normal
			// top-level navigation, not a cross-site request. Browsers still mark it
			// "cross-site", but it also carries Sec-Fetch-Mode: navigate with
			// Sec-Fetch-Dest: document. The threat this guard exists for is a public page
			// making subresource or API requests at this LAN/tailnet service (DNS
			// rebinding, local network access), and that traffic never looks like a
			// navigation. GET/HEAD only, because a cross-site form POST also navigates
			// and must stay out.
			navigation := (r.Method == "GET" || r.Method == "HEAD") &&
				r.Header.Get("Sec-Fetch-Mode") == "navigate" &&
				r.Header.Get("Sec-Fetch-Dest") == "document"
			if !navigation && !((r.Method == "GET" || r.Method == "HEAD") && sandboxReadable(r.URL.Path)) {
				http.Error(w, "cross-site request denied", 403)
				return
			}
		}
		// Last, with every network check passed: the workspace's own sign-in, when a
		// password is set (auth.go). Share and app hostnames are public by design.
		if !appHost && !s.isShareHost(r) {
			var ok bool
			if r, ok = s.signInGate(w, r); !ok {
				return
			}
		}
		defer func() {
			if v := recover(); v != nil {
				log.Printf("request panic: %v", v)
				http.Error(w, "internal error", 500)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
func (s *Server) revision(r *http.Request) (store.Revision, error) {
	a, e := s.Store.Get(r.PathValue("id"))
	if e != nil {
		return store.Revision{}, e
	}
	v := len(a.Revisions)
	if raw := r.URL.Query().Get("version"); raw != "" {
		v, e = strconv.Atoi(raw)
		if e != nil || v < 1 || v > len(a.Revisions) {
			return store.Revision{}, store.ErrNotFound
		}
	}
	return a.Revisions[v-1], nil
}
func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	rev, e := s.revision(r)
	if e != nil {
		respond(w, 404, nil, e)
		return
	}
	ext := map[string]string{"html": "html", "react": "tsx", "markdown": "md", "slides": "md", "svg": "svg", "mermaid": "mmd", "code": "txt"}[rev.Kind]
	if len(rev.Files) == 0 {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="artifact.%s"`, ext))
		w.Write([]byte(rev.Content))
		return
	}
	// With files, a zip of the page (as index.*, or App.tsx for React) and its files.
	main := "index." + ext
	if rev.Kind == "react" {
		main = "App.tsx"
	}
	if _, taken := rev.Files[main]; taken {
		main = "_" + main
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="artifact.zip"`)
	z := zip.NewWriter(w)
	names := append([]string{main}, slices.Sorted(maps.Keys(rev.Files))...)
	for _, name := range names {
		body, ok := rev.Files[name]
		if !ok {
			body = rev.Content
		}
		f, e := z.Create(name)
		if e != nil {
			return
		}
		io.WriteString(f, body)
	}
	z.Close()
}
