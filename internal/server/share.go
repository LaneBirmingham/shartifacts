package server

import (
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/LaneBirmingham/shartifacts/internal/store"
)

// Read-only share links for people outside the network. They are served only on the
// operator's share origin (-share-origin), a hostname the reverse proxy exposes; on that
// hostname this server answers nothing but share pages and the files they load, whatever
// the proxy sends, so the workspace, API and MCP never become public through it.
//
// What a visitor may read there is what a token grants, and every read names the token:
//   - With no token, nothing of any artifact: only the static page shell, runtime and
//     libraries every share page loads (sharePublic). The workspace's bare /media/ and
//     /_blob/ paths answer 404 here, because they carry no token; previews from the
//     opaque-origin sandbox send no Referer or cookie that could carry one.
//   - Under /s/{token}/, the token's artifact and nothing else: the shared revision (the
//     pinned version, or the latest for a link that follows new versions) and its files;
//     the media that revision references (/s/{token}/media/{id}); the artifact's own assets
//     (/s/{token}/_blob/{id}); its shared documents and shared storage, read-only, as a
//     visitor with no identity (no data/users/ subtree, nothing a rule keeps above view, no
//     personal storage); and live notice of changes to exactly those.
// Shared pages are served with their media and asset references pointed at the token's
// paths, and their CSP allows only those. Revoking a link ends all of it at once.

// shareHost is the host[:port] of the share origin, or "".
func (s *Server) shareHost() string {
	if s.ShareOrigin == "" {
		return ""
	}
	u, err := url.Parse(s.ShareOrigin)
	if err != nil {
		return ""
	}
	return u.Host
}

func (s *Server) isShareHost(r *http.Request) bool {
	h := s.shareHost()
	return h != "" && strings.EqualFold(requestHost(r), h)
}

// requestHost is the Host a request named, without a trailing dot or the default port of
// the scheme browsers used, which they leave out and some proxies add.
func requestHost(r *http.Request) string {
	h := strings.ToLower(r.Host)
	if name, port, err := net.SplitHostPort(h); err == nil && (port == "443" || port == "80") {
		h = name
		if strings.Contains(name, ":") {
			h = "[" + name + "]"
		}
	}
	return strings.TrimSuffix(h, ".")
}

// sharePublic lists what a share hostname may serve: GETs of share pages and what they
// load, and the read-only share API.
func sharePublic(r *http.Request) bool {
	p := r.URL.Path
	// Public app links: the link's token is the grant, and the app's backend answers writes.
	if strings.HasPrefix(p, "/p/") {
		return true
	}
	if r.Method == "POST" {
		return strings.HasPrefix(p, "/s/") && (strings.HasSuffix(p, "/api/db") || strings.HasSuffix(p, "/api/storage"))
	}
	if r.Method != "GET" && r.Method != "HEAD" {
		return false
	}
	for _, prefix := range []string{"/s/", "/vendor/"} {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	switch p {
	case "/runtime.js", "/bridge.js", "/icons.js", "/share.js", "/theme.js", "/share.css", "/view.css", "/design-system.css", "/dark.css", "/palettes.css", "/favicon.svg":
		return true
	}
	return false
}

// appHostPublic lists what an app's own hostname (AppOrigin) may serve: its public links,
// and the bundled libraries their pages load from their own origin.
func appHostPublic(r *http.Request) bool {
	p := r.URL.Path
	return strings.HasPrefix(p, "/p/") || (r.Method == "GET" || r.Method == "HEAD") && strings.HasPrefix(p, "/vendor/")
}

// shared resolves a request's token to its revision.
func (s *Server) shared(r *http.Request) (store.Share, store.Artifact, store.Revision, bool) {
	sh, a, ok := s.Store.Shared(r.PathValue("token"))
	if !ok {
		return sh, a, store.Revision{}, false
	}
	v := sh.Version
	if v == 0 || v > len(a.Revisions) {
		v = len(a.Revisions)
	}
	return sh, a, a.Revisions[v-1], true
}

// objectRef finds the media and asset URLs a page loads: /media/<sha256>.<ext>, and
// /_blob/ (an asset id follows, often concatenated in code), after a quote, bracket, = ,
// comma or space.
var objectRef = regexp.MustCompile("(^|[\"'`(=\\s,])/(media/[0-9a-f]{64}\\.[A-Za-z0-9]+|_blob/)")

// scopeRefs points a shared page's media and asset URLs at its token's paths.
func scopeRefs(token, text string) string {
	return objectRef.ReplaceAllString(text, "${1}/s/"+token+"/${2}")
}

// sharedRevision is a revision as a share link serves it.
func sharedRevision(token string, rev store.Revision) store.Revision {
	if rev.Kind == "code" {
		return rev // shown as text; nothing loads
	}
	rev.Content = scopeRefs(token, rev.Content)
	if len(rev.Files) > 0 {
		files := make(map[string]string, len(rev.Files))
		for p, body := range rev.Files {
			files[p] = scopeRefs(token, body)
		}
		rev.Files = files
	}
	return rev
}

// references reports whether a revision names a media file.
func references(rev store.Revision, media string) bool {
	if strings.Contains(rev.Content, "/media/"+media) {
		return true
	}
	for _, body := range rev.Files {
		if strings.Contains(body, "/media/"+media) {
			return true
		}
	}
	return false
}

// scopeData points media and asset URLs held in shared documents at the token's paths.
func scopeData(token string, v any) any {
	switch t := v.(type) {
	case string:
		return scopeRefs(token, t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = scopeData(token, x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = scopeData(token, x)
		}
		return out
	}
	return v
}

func scopeDoc(token string, d store.Doc) store.Doc {
	if d.Data != nil {
		d.Data = scopeData(token, d.Data).(map[string]any)
	}
	return d
}

func (s *Server) shareURL(token string) string {
	if s.ShareOrigin == "" {
		return ""
	}
	return strings.TrimRight(s.ShareOrigin, "/") + "/s/" + token
}

// maxShareBody bounds a share link's read-only data calls, which only name what to read.
const maxShareBody = 64 << 10

func (s *Server) shareRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		mode := "private"
		if s.ShareOrigin != "" {
			mode = "public-sharing"
		}
		writeJSON(w, 200, map[string]any{"mode": mode, "share_origin": s.ShareOrigin, "app_origin": s.AppOrigin, "public_origin": s.PublicOrigin})
	})
	mux.HandleFunc("GET /api/artifacts/{id}/shares", func(w http.ResponseWriter, r *http.Request) {
		if _, e := s.Store.Get(r.PathValue("id")); e != nil {
			fail(w, e)
			return
		}
		out := []map[string]any{}
		for _, sh := range s.Store.Shares(r.PathValue("id")) {
			out = append(out, map[string]any{"token": sh.Token, "version": sh.Version, "created_at": sh.CreatedAt, "url": s.shareURL(sh.Token)})
		}
		writeJSON(w, 200, map[string]any{"shares": out})
	})
	mux.HandleFunc("POST /api/artifacts/{id}/shares", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			// Required: a pinned version, or 0 for a link that follows new versions.
			Version *int `json:"version"`
		}
		if !decode(w, r, &in) {
			return
		}
		if in.Version == nil {
			writeJSON(w, 400, map[string]string{"error": "version is required: the version to share, or 0 for a link that follows new versions"})
			return
		}
		if s.ShareOrigin == "" {
			writeJSON(w, 400, map[string]string{"error": "sharing outside the network is off: set SHARTIFACTS_SHARE_ORIGIN (-share-origin) to a public hostname of your own; see docs/sharing.md"})
			return
		}
		sh, e := s.Store.CreateShare(r.PathValue("id"), *in.Version)
		if e != nil {
			fail(w, e)
			return
		}
		writeJSON(w, 201, map[string]any{"token": sh.Token, "version": sh.Version, "created_at": sh.CreatedAt, "url": s.shareURL(sh.Token)})
	})
	mux.HandleFunc("DELETE /api/shares/{token}", func(w http.ResponseWriter, r *http.Request) {
		revoked, e := s.Store.RevokeShare(r.PathValue("token"))
		respond(w, 200, map[string]bool{"revoked": revoked}, e)
	})

	// ---- served on the share origin --------------------------------------------------
	mux.HandleFunc("GET /s/{token}", func(w http.ResponseWriter, r *http.Request) {
		if _, _, _, ok := s.shared(r); !ok {
			http.Error(w, "This link was revoked or never existed.", 404)
			return
		}
		b, e := fs.ReadFile(s.Web, "share.html")
		if e != nil {
			http.Error(w, "unavailable", 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; frame-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		w.Write(b)
	})
	mux.HandleFunc("GET /s/{token}/meta", func(w http.ResponseWriter, r *http.Request) {
		sh, a, rev, ok := s.shared(r)
		if !ok {
			http.NotFound(w, r)
			return
		}
		m := map[string]any{"title": rev.Title, "kind": rev.Kind, "version": rev.Version, "follows_latest": sh.Version == 0, "files": len(rev.Files) > 0, "description": "", "icon": ""}
		if sh.Version == 0 {
			// Description and icon are the artifact's current ones, not any version's.
			m["description"], m["icon"] = a.Description, a.Icon
		}
		writeJSON(w, 200, m)
	})
	mux.HandleFunc("GET /s/{token}/preview", func(w http.ResponseWriter, r *http.Request) {
		_, _, rev, ok := s.shared(r)
		if !ok {
			http.NotFound(w, r)
			return
		}
		if len(rev.Files) > 0 {
			http.Redirect(w, r, "/s/"+r.PathValue("token")+"/preview/", http.StatusFound)
			return
		}
		token := r.PathValue("token")
		s.renderPreview(w, r, sharedRevision(token, rev), "", "/s/"+token)
	})
	mux.HandleFunc("GET /s/{token}/preview/{path...}", func(w http.ResponseWriter, r *http.Request) {
		_, _, rev, ok := s.shared(r)
		if !ok {
			http.NotFound(w, r)
			return
		}
		token := r.PathValue("token")
		s.serveArtifactPath(w, r, sharedRevision(token, rev), "/s/"+token+"/preview/", "/s/"+token)
	})
	// Media the shared revision references, and the shared artifact's own assets.
	mux.HandleFunc("GET /s/{token}/media/{id}", func(w http.ResponseWriter, r *http.Request) {
		_, _, rev, ok := s.shared(r)
		if !ok || !references(rev, r.PathValue("id")) {
			http.NotFound(w, r)
			return
		}
		s.serveMedia(w, r, r.PathValue("id"))
	})
	mux.HandleFunc("GET /s/{token}/_blob/{id}", func(w http.ResponseWriter, r *http.Request) {
		_, a, _, ok := s.shared(r)
		if !ok {
			http.NotFound(w, r)
			return
		}
		path, typ, e := s.Store.ArtifactAssetFile(a.ID, r.PathValue("id"))
		if e != nil {
			http.NotFound(w, r)
			return
		}
		serveAsset(w, r, path, typ)
	})
	mux.HandleFunc("GET /s/{token}/api/me", func(w http.ResponseWriter, r *http.Request) {
		if _, _, _, ok := s.shared(r); !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, 200, map[string]any{"id": nil, "name": "", "color": "", "isOwner": false, "canEdit": false})
	})
	mux.HandleFunc("POST /s/{token}/api/db", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Op    string         `json:"op"`
			Path  string         `json:"path"`
			Query store.DocQuery `json:"query,omitempty"`
			// Accepted only to refuse writes plainly (below).
			Data   json.RawMessage `json:"data,omitempty"`
			Holder string          `json:"holder,omitempty"`
			TTLMs  int64           `json:"ttlMs,omitempty"`
		}
		// Anyone on the internet can call this: find the link before reading a small body.
		_, a, _, ok := s.shared(r)
		if !ok {
			http.NotFound(w, r)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxShareBody)
		if !decode(w, r, &in) {
			return
		}
		visitor, token := store.Viewer{ReadOnly: true}, r.PathValue("token")
		switch in.Op {
		case "get":
			d, e := s.Store.DocGet(a.ID, in.Path, visitor)
			respond(w, 200, scopeDoc(token, d), e)
		case "query":
			docs, e := s.Store.DocQuery(a.ID, in.Path, in.Query, visitor)
			for i := range docs {
				docs[i] = scopeDoc(token, docs[i])
			}
			respond(w, 200, map[string]any{"docs": docs}, e)
		default:
			writeJSON(w, 403, map[string]string{"code": "not_granted", "error": "shared links are read-only"})
		}
	})
	mux.HandleFunc("POST /s/{token}/api/storage", func(w http.ResponseWriter, r *http.Request) {
		var in storageOp
		_, a, _, ok := s.shared(r)
		if !ok {
			http.NotFound(w, r)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxShareBody)
		if !decode(w, r, &in) {
			return
		}
		if in.Op != "get" && in.Op != "list" {
			writeJSON(w, 403, map[string]string{"error": "shared links are read-only"})
			return
		}
		// Personal storage is the person's own; a visitor has none, as in Claude.
		if !in.Shared {
			if in.Op == "list" {
				writeJSON(w, 200, map[string]any{"keys": []string{}, "prefix": in.Prefix, "shared": false})
			} else {
				writeJSON(w, 200, nil)
			}
			return
		}
		result, e := s.runStorage(a.ID, in)
		if v, ok := result["value"].(string); ok {
			result["value"] = scopeRefs(r.PathValue("token"), v)
		}
		if e == nil && result == nil {
			writeJSON(w, 200, nil)
			return
		}
		respond(w, 200, result, e)
	})
	mux.HandleFunc("GET /s/{token}/api/live", func(w http.ResponseWriter, r *http.Request) {
		sh, a, _, ok := s.shared(r)
		if !ok {
			http.NotFound(w, r)
			return
		}
		s.liveFor(w, r, a.ID, "", &sh)
	})
}
