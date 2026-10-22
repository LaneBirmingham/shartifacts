package server

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"maps"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LaneBirmingham/shartifacts/internal/runner"
	"github.com/LaneBirmingham/shartifacts/internal/store"
)

// deployInput is a deployment as JSON (the MCP tools and the hub send this).
type deployInput struct {
	// Files are text files by path; BinaryFiles are base64 (images, a small prebuilt binary).
	Files       map[string]string `json:"files,omitempty"`
	BinaryFiles map[string]string `json:"binary_files,omitempty"`
	// Remove deletes paths from the previous version; Edits change its text files.
	Remove []string     `json:"remove,omitempty"`
	Edits  []store.Edit `json:"edits,omitempty"`
	// Merge starts from the latest version's files instead of an empty tree.
	Merge       bool   `json:"merge,omitempty"`
	Note        string `json:"note,omitempty"`
	Promote     *bool  `json:"promote,omitempty"`
	BaseVersion *int   `json:"base_version,omitempty"`
	// Template starts a new app from a built-in example ("notes").
	Template string `json:"template,omitempty"`
}

func (in deployInput) stage() (store.StageInput, error) {
	out := store.StageInput{Merge: in.Merge, Remove: in.Remove, Edits: in.Edits, Note: in.Note, BaseVersion: -1, Files: map[string][]byte{}}
	if in.BaseVersion != nil {
		out.BaseVersion = *in.BaseVersion
	}
	for p, body := range in.Files {
		out.Files[p] = []byte(body)
	}
	for p, b64 := range in.BinaryFiles {
		if _, dup := out.Files[p]; dup {
			return out, fmt.Errorf("%s is in both files and binary_files", p)
		}
		b, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return out, fmt.Errorf("binary_files %s: invalid base64: %v", p, err)
		}
		out.Files[p] = b
	}
	if in.Template != "" {
		files, err := templateFiles(in.Template)
		if err != nil {
			return out, err
		}
		for p, b := range files {
			if _, given := out.Files[p]; !given {
				out.Files[p] = b
			}
		}
	}
	return out, nil
}

// DeployWait is how long a deploy request waits for its build before answering "building".
const DeployWait = 110 * time.Second

// deploy stages a version, builds it in the background and waits up to wait for the result.
func (s *Server) deploy(id string, in store.StageInput, promote bool, wait time.Duration) (store.Deployment, store.App, error) {
	d, err := s.Store.StageDeployment(id, in)
	if err != nil {
		return store.Deployment{}, store.App{}, err
	}
	done := make(chan store.App, 1)
	go func() {
		mu, _ := s.appBuilds.LoadOrStore(id, &sync.Mutex{})
		mu.(*sync.Mutex).Lock()
		defer mu.(*sync.Mutex).Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), runner.BuildTimeout+2*time.Minute)
		defer cancel()
		result := s.buildDeployment(ctx, id, d)
		before, _ := s.Store.App(id)
		a, err := s.Store.FinishDeployment(id, result, promote)
		if err != nil {
			log.Printf("app %s v%d: %v", id, d.Version, err)
			a, _ = s.Store.App(id)
		} else {
			s.retire(id, before.Live, a.Live)
		}
		if !promote && result.Status == store.StatusReady {
			// A staged version was started only to prove it runs.
			s.Runtime.Stop(context.Background(), id, d.Version)
		}
		done <- a
	}()
	select {
	case a := <-done:
		if d.Version <= len(a.Deployments) {
			d = a.Deployments[d.Version-1]
		}
		return d, a, nil
	case <-time.After(wait):
		a, err := s.Store.App(id)
		return d, a, err
	}
}

// appView is an app as the hub and agents see it: env names without values, links resolved.
func (s *Server) appView(a store.App, base string, logs bool) map[string]any {
	deps := make([]store.Deployment, len(a.Deployments))
	copy(deps, a.Deployments)
	if !logs {
		for i := range deps {
			deps[i].Log = ""
		}
	}
	slices.Reverse(deps)
	shares := []map[string]any{}
	for _, sh := range a.Shares {
		shares = append(shares, map[string]any{"token": sh.Token, "version": sh.Version, "read_only": sh.ReadOnly, "created_at": sh.CreatedAt, "url": s.appShareURL(a, sh.Token)})
	}
	env := slices.Sorted(maps.Keys(a.Env))
	if env == nil {
		env = []string{}
	}
	out := map[string]any{
		"id": a.ID, "name": a.Name, "description": a.Description, "icon": a.Icon, "pinned": a.Pinned, "label": a.Label,
		"created_at": a.CreatedAt, "updated_at": a.UpdatedAt, "live": a.Live, "latest": len(a.Deployments),
		"env": env, "shares": shares, "deployments": deps,
		// Rollback reaches back to oldest_rollback; versions before it lost their files.
		"keep_versions": s.Store.KeepVersionsOrDefault(), "oldest_rollback": a.OldestRollback(),
		"hub_url": base + "/apps/" + a.ID,
	}
	if s.ShareOrigin != "" && s.AppOrigin != "" {
		out["link_origin"] = s.appLinkOrigin(a)
	}
	if a.Live > 0 {
		out["live_url"] = appURL(base, a.ID, a.Live, true)
		out["api_url"] = appURL(base, a.ID, a.Live, true) + "api/"
	}
	return out
}

func (s *Server) appRoutes(mux *http.ServeMux) {
	s.appServeRoutes(mux)
	origin := func(r *http.Request) string { return s.clientScheme(r) + "://" + r.Host }
	mux.HandleFunc("GET /api/apps", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"apps": s.Store.Apps(), "runtime": s.Runtime.Info(r.Context()), "share_origin": s.ShareOrigin, "app_origin": s.AppOrigin})
	})
	mux.HandleFunc("GET /api/apps/{id}/usage", func(w http.ResponseWriter, r *http.Request) {
		u, err := s.Store.AppUsage(r.PathValue("id"))
		if err != nil {
			respond(w, 200, nil, err)
			return
		}
		// The quota is the runner's to enforce, so it reports it.
		writeJSON(w, 200, struct {
			store.AppUsage
			KeepVersions int   `json:"keep_versions"`
			DataQuota    int64 `json:"data_quota"`
		}{u, s.Store.KeepVersionsOrDefault(), s.Runtime.Info(r.Context()).DataQuota})
	})
	mux.HandleFunc("POST /api/apps", func(w http.ResponseWriter, r *http.Request) {
		var meta store.AppMeta
		var in deployInput
		var stage store.StageInput
		var err error
		if archive(r) {
			q := r.URL.Query()
			name, desc, icon, label := q.Get("name"), q.Get("description"), q.Get("icon"), q.Get("label")
			meta = store.AppMeta{Name: &name, Description: &desc, Icon: &icon, Label: &label}
			if stage, err = archiveStage(w, r); err != nil {
				writeJSON(w, 400, map[string]string{"error": err.Error()})
				return
			}
			stage.BaseVersion, stage.Note = 0, q.Get("note")
		} else {
			var body struct {
				store.AppMeta
				deployInput
			}
			if !decode(w, r, &body) {
				return
			}
			meta, in = body.AppMeta, body.deployInput
			if stage, err = in.stage(); err != nil {
				writeJSON(w, 400, map[string]string{"error": err.Error()})
				return
			}
			stage.BaseVersion = 0
		}
		a, err := s.createApp(meta)
		if err != nil {
			respond(w, 400, nil, err)
			return
		}
		if len(stage.Files) == 0 {
			writeJSON(w, 201, map[string]any{"app": s.appView(a, origin(r), false)})
			return
		}
		d, a, err := s.deploy(a.ID, stage, true, DeployWait)
		if err != nil {
			s.Store.DeleteApp(a.ID)
			respond(w, 400, nil, err)
			return
		}
		writeJSON(w, 201, map[string]any{"app": s.appView(a, origin(r), false), "deployment": d})
	})
	mux.HandleFunc("GET /api/apps/guide", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, s.appGuide(r.Context(), origin(r)))
	})
	mux.HandleFunc("GET /api/apps/{id}", func(w http.ResponseWriter, r *http.Request) {
		a, err := s.Store.App(r.PathValue("id"))
		if err != nil {
			respond(w, 404, nil, err)
			return
		}
		writeJSON(w, 200, s.appView(a, origin(r), r.URL.Query().Has("logs")))
	})
	mux.HandleFunc("PATCH /api/apps/{id}", func(w http.ResponseWriter, r *http.Request) {
		var meta store.AppMeta
		if !decode(w, r, &meta) {
			return
		}
		a, err := s.updateApp(r.PathValue("id"), meta)
		if err != nil {
			respond(w, 400, nil, err)
			return
		}
		writeJSON(w, 200, s.appView(a, origin(r), false))
	})
	mux.HandleFunc("DELETE /api/apps/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, err := s.Store.App(id); err != nil {
			respond(w, 404, nil, err)
			return
		}
		s.Runtime.Stop(r.Context(), id, 0)
		respond(w, 200, map[string]bool{"deleted": true}, s.Store.DeleteApp(id))
	})
	mux.HandleFunc("POST /api/apps/{id}/deployments", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, err := s.Store.App(id); err != nil {
			respond(w, 404, nil, err)
			return
		}
		promote := true
		var stage store.StageInput
		var err error
		if archive(r) {
			q := r.URL.Query()
			if stage, err = archiveStage(w, r); err != nil {
				writeJSON(w, 400, map[string]string{"error": err.Error()})
				return
			}
			stage.BaseVersion, stage.Note, stage.Merge = -1, q.Get("note"), q.Get("merge") == "true"
			if v := q.Get("base_version"); v != "" {
				if stage.BaseVersion, err = strconv.Atoi(v); err != nil {
					writeJSON(w, 400, map[string]string{"error": "base_version must be a number"})
					return
				}
			}
			promote = q.Get("promote") != "false"
		} else {
			var in deployInput
			if !decode(w, r, &in) {
				return
			}
			if stage, err = in.stage(); err != nil {
				writeJSON(w, 400, map[string]string{"error": err.Error()})
				return
			}
			if in.Promote != nil {
				promote = *in.Promote
			}
		}
		d, a, err := s.deploy(id, stage, promote, DeployWait)
		if err != nil {
			respond(w, 400, nil, err)
			return
		}
		status := 201
		if d.Status == store.StatusBuilding {
			status = 202
		}
		writeJSON(w, status, map[string]any{"deployment": d, "app": s.appView(a, origin(r), false)})
	})
	mux.HandleFunc("GET /api/apps/{id}/deployments/{version}/files", func(w http.ResponseWriter, r *http.Request) {
		v, _ := strconv.Atoi(r.PathValue("version"))
		files, err := s.Store.AppFiles(r.PathValue("id"), v)
		respond(w, 200, map[string]any{"files": files}, err)
	})
	mux.HandleFunc("GET /api/apps/{id}/deployments/{version}/files/{path...}", func(w http.ResponseWriter, r *http.Request) {
		v, _ := strconv.Atoi(r.PathValue("version"))
		b, err := s.Store.ReadAppFile(r.PathValue("id"), v, r.PathValue("path"))
		if err != nil {
			respond(w, 404, nil, err)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
		w.Write(b)
	})
	mux.HandleFunc("GET /api/apps/{id}/download", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		a, err := s.Store.App(id)
		if err != nil {
			respond(w, 404, nil, err)
			return
		}
		v, _ := strconv.Atoi(r.URL.Query().Get("version"))
		if v == 0 {
			v = len(a.Deployments)
		}
		files, err := s.Store.AppFiles(id, v)
		if err != nil {
			respond(w, 404, nil, err)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="app-v%d.zip"`, v))
		z := zip.NewWriter(w)
		for _, p := range slices.Sorted(maps.Keys(files)) {
			b, err := s.Store.ReadAppFile(id, v, p)
			if err != nil {
				break
			}
			f, err := z.Create(p)
			if err != nil {
				break
			}
			f.Write(b)
		}
		z.Close()
	})
	mux.HandleFunc("PUT /api/apps/{id}/live", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Version int `json:"version"`
		}
		if !decode(w, r, &in) {
			return
		}
		a, err := s.promote(r.PathValue("id"), in.Version)
		if err != nil {
			respond(w, 400, nil, err)
			return
		}
		writeJSON(w, 200, s.appView(a, origin(r), false))
	})
	mux.HandleFunc("GET /api/apps/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.appLogs(r.Context(), r.PathValue("id"), atoi(r.URL.Query().Get("version")), atoi(r.URL.Query().Get("tail")))
		respond(w, 200, out, err)
	})
	mux.HandleFunc("POST /api/apps/{id}/restart", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, err := s.Store.App(id); err != nil {
			respond(w, 404, nil, err)
			return
		}
		respond(w, 200, map[string]bool{"restarted": true}, s.Runtime.Stop(r.Context(), id, 0))
	})
	mux.HandleFunc("PUT /api/apps/{id}/env", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Vars map[string]*string `json:"vars"`
		}
		if !decode(w, r, &in) {
			return
		}
		a, err := s.setAppEnv(r.Context(), r.PathValue("id"), in.Vars)
		if err != nil {
			respond(w, 400, nil, err)
			return
		}
		writeJSON(w, 200, s.appView(a, origin(r), false))
	})
	mux.HandleFunc("POST /api/apps/{id}/shares", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Version  *int `json:"version"`
			ReadOnly bool `json:"read_only"`
		}
		if !decode(w, r, &in) {
			return
		}
		if in.Version == nil {
			writeJSON(w, 400, map[string]string{"error": "version is required: the version to share, or 0 for a link that follows the live version"})
			return
		}
		sh, err := s.createAppShare(r.PathValue("id"), *in.Version, in.ReadOnly)
		if err != nil {
			respond(w, 400, nil, err)
			return
		}
		writeJSON(w, 201, sh)
	})
	mux.HandleFunc("DELETE /api/app-shares/{token}", func(w http.ResponseWriter, r *http.Request) {
		revoked, err := s.Store.RevokeAppShare(r.PathValue("token"))
		respond(w, 200, map[string]bool{"revoked": revoked}, err)
	})
	mux.HandleFunc("POST /api/apps/{id}/errors", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Version int    `json:"version"`
			Message string `json:"message"`
		}
		if !decode(w, r, &in) {
			return
		}
		if _, err := s.Store.App(r.PathValue("id")); err != nil {
			respond(w, 404, nil, err)
			return
		}
		s.appErrors.add(r.PathValue("id"), in.Version, in.Message)
		writeJSON(w, 202, map[string]bool{"recorded": true})
	})
}

// RetireDelay is how long a version that stopped being live keeps serving the requests
// already on their way to it before it is stopped.
var RetireDelay = 15 * time.Second

// retire stops the version that was live once another one has replaced it, unless it is
// live again by then. Previews start it again on demand.
func (s *Server) retire(id string, previous, live int) {
	if previous == 0 || previous == live {
		return
	}
	time.AfterFunc(RetireDelay, func() {
		if a, err := s.Store.App(id); err == nil && a.Live != previous {
			s.Runtime.Stop(context.Background(), id, previous)
		}
	})
}

// promote makes a version live and retires the one it replaces.
func (s *Server) promote(id string, version int) (store.App, error) {
	before, err := s.Store.App(id)
	if err != nil {
		return store.App{}, err
	}
	a, err := s.Store.SetLive(id, version)
	if err == nil {
		s.retire(id, before.Live, a.Live)
	}
	return a, err
}

func atoi(v string) int {
	n, _ := strconv.Atoi(v)
	return n
}

func (s *Server) setAppEnv(ctx context.Context, id string, vars map[string]*string) (store.App, error) {
	a, err := s.Store.SetAppEnv(id, vars)
	if err == nil {
		// Backends read their environment at start; the next request starts them afresh.
		s.Runtime.Stop(ctx, id, 0)
	}
	return a, err
}

func (s *Server) createAppShare(id string, version int, readOnly bool) (map[string]any, error) {
	if s.ShareOrigin == "" {
		return nil, errors.New("public links are off: set SHARTIFACTS_SHARE_ORIGIN (-share-origin) to a public hostname of your own and route it here; see docs/sharing.md")
	}
	sh, err := s.Store.CreateAppShare(id, version, readOnly)
	if err != nil {
		return nil, err
	}
	a, err := s.Store.App(id)
	if err != nil {
		return nil, err
	}
	return map[string]any{"token": sh.Token, "version": sh.Version, "read_only": sh.ReadOnly, "created_at": sh.CreatedAt, "url": s.appShareURL(a, sh.Token)}, nil
}

// createApp and updateApp are the store's, with the label checks only the server can make.
func (s *Server) createApp(m store.AppMeta) (store.App, error) {
	if err := s.checkAppLabel(m.Label); err != nil {
		return store.App{}, err
	}
	return s.Store.CreateApp(m)
}

func (s *Server) updateApp(id string, m store.AppMeta) (store.App, error) {
	if err := s.checkAppLabel(m.Label); err != nil {
		return store.App{}, err
	}
	return s.Store.UpdateApp(id, m)
}

// appLogs are a version's backend output and the errors browsers met in its pages.
func (s *Server) appLogs(ctx context.Context, id string, version, tail int) (map[string]any, error) {
	a, err := s.Store.App(id)
	if err != nil {
		return nil, err
	}
	if version == 0 {
		version = a.Live
	}
	if version == 0 {
		version = len(a.Deployments)
	}
	if version < 1 || version > len(a.Deployments) {
		return nil, fmt.Errorf("version %d does not exist", version)
	}
	d := a.Deployments[version-1]
	out := map[string]any{"version": version, "status": d.Status, "build_error": d.Error, "build_log": d.Log, "frontend_errors": s.appErrors.forVersion(id, version)}
	if d.Backend != "" {
		lg, err := s.Runtime.Logs(ctx, id, version, tail)
		if err != nil {
			out["backend_error"] = err.Error()
		} else {
			out["backend"] = lg
		}
	}
	return out, nil
}

// appErrorLog keeps the latest errors browsers reported from app pages, in memory.
type appErrorLog struct {
	mu    sync.Mutex
	items map[string][]appError
}

type appError struct {
	Version int       `json:"version"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

func (l *appErrorLog) add(id string, version int, msg string) {
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.items == nil {
		l.items = map[string][]appError{}
	}
	list := l.items[id]
	for _, e := range list {
		if e.Version == version && e.Message == msg {
			return
		}
	}
	list = append(list, appError{version, msg, time.Now().UTC()})
	if len(list) > 50 {
		list = list[len(list)-50:]
	}
	l.items[id] = list
}

func (l *appErrorLog) forVersion(id string, version int) []appError {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []appError{}
	for _, e := range l.items[id] {
		if e.Version == version {
			out = append(out, e)
		}
	}
	return out
}

func archive(r *http.Request) bool {
	t := r.Header.Get("Content-Type")
	for _, a := range []string{"application/zip", "application/x-zip-compressed", "application/gzip", "application/x-gzip", "application/x-tar", "application/x-gtar"} {
		if strings.HasPrefix(t, a) {
			return true
		}
	}
	return false
}

// archiveStage reads a zip or (gzipped) tar body into a source tree. A single top-level
// directory holding everything (zip -r app.zip myapp) is unwrapped.
func archiveStage(w http.ResponseWriter, r *http.Request) (store.StageInput, error) {
	r.Body = http.MaxBytesReader(w, r.Body, store.MaxAppBytes+(16<<20))
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return store.StageInput{}, fmt.Errorf("read upload: %v", err)
	}
	files := map[string][]byte{}
	var total int64
	add := func(name string, mode fs.FileMode, rd io.Reader) error {
		name = strings.TrimPrefix(path.Clean("/"+strings.ReplaceAll(name, "\\", "/")), "/")
		if !mode.IsRegular() || name == "" {
			return nil
		}
		base := path.Base(name)
		// Ignore OS and editor litter rather than refusing the upload.
		if strings.HasPrefix(name, "__MACOSX/") || base == ".DS_Store" || strings.Contains("/"+name, "/.git/") {
			return nil
		}
		b, err := io.ReadAll(io.LimitReader(rd, store.MaxAppFileBytes+1))
		if err != nil {
			return err
		}
		total += int64(len(b))
		if len(files) >= store.MaxAppFiles || total > store.MaxAppBytes {
			return fmt.Errorf("the archive exceeds %d files or %d MiB", store.MaxAppFiles, store.MaxAppBytes>>20)
		}
		files[name] = b
		return nil
	}
	if bytes.HasPrefix(body, []byte("PK")) {
		zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
		if err != nil {
			return store.StageInput{}, fmt.Errorf("read zip: %v", err)
		}
		for _, f := range zr.File {
			rc, err := f.Open()
			if err != nil {
				return store.StageInput{}, fmt.Errorf("%s: %v", f.Name, err)
			}
			err = add(f.Name, f.Mode(), rc)
			rc.Close()
			if err != nil {
				return store.StageInput{}, err
			}
		}
	} else {
		var rd io.Reader = bytes.NewReader(body)
		if bytes.HasPrefix(body, []byte{0x1f, 0x8b}) {
			gz, err := gzip.NewReader(rd)
			if err != nil {
				return store.StageInput{}, fmt.Errorf("read gzip: %v", err)
			}
			rd = gz
		}
		tr := tar.NewReader(rd)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return store.StageInput{}, fmt.Errorf("read tar: %v", err)
			}
			if err = add(h.Name, h.FileInfo().Mode(), tr); err != nil {
				return store.StageInput{}, err
			}
		}
	}
	if len(files) == 0 {
		return store.StageInput{}, errors.New("the archive holds no files")
	}
	// Unwrap one directory that holds everything.
	top := ""
	for p := range files {
		dir, _, nested := strings.Cut(p, "/")
		if !nested || (top != "" && dir != top) {
			top = ""
			break
		}
		top = dir
	}
	if top != "" {
		unwrapped := map[string][]byte{}
		for p, b := range files {
			unwrapped[strings.TrimPrefix(p, top+"/")] = b
		}
		files = unwrapped
	}
	return store.StageInput{Files: files}, nil
}
