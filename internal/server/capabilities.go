package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/LaneBirmingham/shartifacts/internal/store"
)

// HTTP endpoints behind the capability runtime (web/runtime.js, answered for each preview
// by web/bridge.js in the page embedding it). They act as the person the request names:
// people say who they are on this trusted network (X-Shartifacts-Viewer), which keeps
// their private data apart without pretending to be authentication.

// viewerID is the person a request acts as, or "" for nobody known.
func (s *Server) viewerID(r *http.Request) string {
	id := r.Header.Get("X-Shartifacts-Viewer")
	if id == "" {
		id = r.URL.Query().Get("viewer")
	}
	if _, ok := s.Store.Person(id); !ok {
		return ""
	}
	return id
}

// viewer is who a request acts as on one artifact. An artifact nobody owns (an agent or
// the API made it) counts everyone as its owner.
func (s *Server) viewer(r *http.Request, a store.Artifact) store.Viewer {
	id := s.viewerID(r)
	return store.Viewer{ID: id, Owner: a.Owner == "" || (id != "" && a.Owner == id)}
}

// codeFor turns store errors into the capability error codes pages branch on.
func codeFor(e error) (int, string) {
	switch {
	case errors.Is(e, store.ErrNotFound):
		return 404, "not_found"
	case errors.Is(e, store.ErrInvalidArgument):
		return 400, "invalid_argument"
	case errors.Is(e, store.ErrQuotaExceeded):
		return 400, "quota_exceeded"
	case errors.Is(e, store.ErrUnsupportedType):
		return 400, "unsupported_type"
	case errors.Is(e, store.ErrTooLarge):
		return 400, "too_large"
	case errors.Is(e, store.ErrConflict):
		return 409, "conflict"
	}
	return 400, "invalid_argument"
}

func fail(w http.ResponseWriter, e error) {
	status, code := codeFor(e)
	msg := strings.TrimPrefix(e.Error(), code+": ")
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}

func (s *Server) capabilityRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/people", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"people": s.Store.People()})
	})
	mux.HandleFunc("POST /api/people", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name string `json:"name"`
		}
		if !decode(w, r, &in) {
			return
		}
		p, e := s.Store.AddPerson(in.Name)
		respond(w, 201, p, e)
	})
	// The `user` capability's view of the person on one artifact.
	mux.HandleFunc("GET /api/artifacts/{id}/me", func(w http.ResponseWriter, r *http.Request) {
		a, e := s.Store.Get(r.PathValue("id"))
		if e != nil {
			fail(w, e)
			return
		}
		v := s.viewer(r, a)
		p, _ := s.Store.Person(v.ID)
		writeJSON(w, 200, map[string]any{"id": nilIfEmpty(p.ID), "name": p.Name, "color": p.Color, "isOwner": v.Owner, "canEdit": true})
	})
	mux.HandleFunc("POST /api/artifacts/{id}/db", s.db)
	mux.HandleFunc("POST /api/artifacts/{id}/assets", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 21<<20)
		a, e := s.Store.UploadAsset(r.PathValue("id"), r.Header.Get("Content-Type"), s.viewerID(r), r.Body)
		if e != nil {
			fail(w, e)
			return
		}
		s.hub.changed(r.PathValue("id"), "assets", a.ID)
		writeJSON(w, 201, a)
	})
	mux.HandleFunc("GET /api/artifacts/{id}/assets", func(w http.ResponseWriter, r *http.Request) {
		list, usage, e := s.Store.Assets(r.PathValue("id"))
		if e != nil {
			fail(w, e)
			return
		}
		writeJSON(w, 200, map[string]any{"assets": list, "usage": usage})
	})
	mux.HandleFunc("DELETE /api/artifacts/{id}/assets/{asset}", func(w http.ResponseWriter, r *http.Request) {
		ref := strings.TrimPrefix(r.PathValue("asset"), "_blob/")
		deleted, e := s.Store.DeleteAsset(r.PathValue("id"), ref)
		if e != nil {
			fail(w, e)
			return
		}
		if deleted {
			s.hub.changed(r.PathValue("id"), "assets", ref)
		}
		writeJSON(w, 200, map[string]bool{"deleted": deleted})
	})
	// Assets are served by id from one root path, which every page of an artifact can use.
	mux.HandleFunc("GET /_blob/{id}", func(w http.ResponseWriter, r *http.Request) {
		path, typ, e := s.Store.AssetFile(r.PathValue("id"))
		if e != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
		serveAsset(w, r, path, typ)
	})
	mux.HandleFunc("POST /api/artifacts/{id}/publish", s.publish)
	mux.HandleFunc("POST /api/sample", s.sample)
	mux.HandleFunc("GET /api/sample/limits", s.sampleLimits)
	mux.HandleFunc("GET /api/artifacts/{id}/live", s.live)
	mux.HandleFunc("POST /api/artifacts/{id}/room", s.room)
}

func serveAsset(w http.ResponseWriter, r *http.Request, path, typ string) {
	w.Header().Set("Content-Type", typ)
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	http.ServeFile(w, r, path)
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// db runs one `db` capability call: POST /api/artifacts/{id}/db
func (s *Server) db(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Op     string         `json:"op"`
		Path   string         `json:"path"`
		Data   map[string]any `json:"data,omitempty"`
		Query  store.DocQuery `json:"query,omitempty"`
		Holder string         `json:"holder,omitempty"`
		TTLMs  int64          `json:"ttlMs,omitempty"`
	}
	if !decode(w, r, &in) {
		return
	}
	a, e := s.Store.Get(r.PathValue("id"))
	if e != nil {
		fail(w, e)
		return
	}
	v := s.viewer(r, a)
	switch in.Op {
	case "get":
		d, e := s.Store.DocGet(a.ID, in.Path, v)
		if e != nil {
			fail(w, e)
			return
		}
		writeJSON(w, 200, d)
	case "query":
		docs, e := s.Store.DocQuery(a.ID, in.Path, in.Query, v)
		if e != nil {
			fail(w, e)
			return
		}
		writeJSON(w, 200, map[string]any{"docs": docs})
	case "set", "update", "delete":
		if e := s.Store.DocWrite(a.ID, in.Op, in.Path, in.Data, v); e != nil {
			fail(w, e)
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	case "acquire":
		res, e := s.Store.DocAcquire(a.ID, in.Path, in.Holder, time.Duration(in.TTLMs)*time.Millisecond, in.Data, v)
		if e != nil {
			fail(w, e)
			return
		}
		writeJSON(w, 200, res)
	default:
		writeJSON(w, 400, map[string]string{"error": "op must be get, query, set, update, delete or acquire", "code": "invalid_argument"})
	}
}
