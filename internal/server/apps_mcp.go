package server

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/LaneBirmingham/shartifacts/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

//go:embed apptemplates
var appTemplates embed.FS

// templateFiles returns a built-in example app's source tree.
func templateFiles(name string) (map[string][]byte, error) {
	if name != "notes" {
		return nil, fmt.Errorf("unknown template %q; available: notes", name)
	}
	root := "apptemplates/" + name
	files := map[string][]byte{"go.mod": []byte("module notes\n\ngo 1.25\n")}
	err := fs.WalkDir(appTemplates, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := appTemplates.ReadFile(p)
		files[strings.TrimPrefix(p, root+"/")] = b
		return err
	})
	return files, err
}

// appGuide is the build contract get_app returns with guide:true, with this server's facts filled in.
func (s *Server) appGuide(ctx context.Context, base string) string {
	info := s.Runtime.Info(ctx)
	goLine := "NOT AVAILABLE on this server (" + map[bool]string{true: "the runner sidecar has no Go toolchain", false: "no Go toolchain and no runner sidecar; see the README"}[info.Remote] + "); deploy prebuilt binaries"
	if info.Go != "" {
		goLine = "built here with " + info.Go + " for " + info.OS + "/" + info.Arch + ", CGO disabled"
	}
	if info.Error != "" {
		goLine = "UNKNOWN: the app runner is unreachable (" + info.Error + ")"
	}
	where := "beside shartifacts"
	if info.Remote {
		where = "in the runner sidecar, which sees only the apps directory"
	}
	network := "enforced"
	if !info.Sandbox.Network {
		network = "NOT enforced on this server (no Landlock network support), so treat it as documentation"
	}
	share := "Public links are OFF on this server (its operator sets SHARTIFACTS_SHARE_ORIGIN to turn them on; see docs/sharing.md)."
	storage := "- Browser storage: in hub tabs localStorage persists per app in that browser, and sessionStorage until the tab closes; elsewhere (public links, a page opened on its own) both last only for the page. No cookies or IndexedDB. Keep state that matters, or that people share, in the backend."
	if s.ShareOrigin != "" {
		share = "Public links are served at " + s.ShareOrigin + "/p/TOKEN/."
	}
	if s.ShareOrigin != "" && s.AppOrigin != "" {
		share = "Public links are served at " + s.AppOrigin + "/p/TOKEN/, one hostname per app: * is a generated label unless you give the app one that reads well (deploy_app label when creating, or manage_app op update label), e.g. budget makes " + strings.Replace(s.AppOrigin, "*", "budget", 1) + "/p/TOKEN/. Choose it before sharing: visitors' browser storage and cookies stay with the hostname they used, though its links keep working (they redirect)."
		storage = "- Browser storage: public links give each app an origin of its own, so localStorage, sessionStorage, IndexedDB and cookies (document.cookie, and the backend's Set-Cookie, kept to that hostname) persist in each visitor's browser as on any site, apart from other apps. Name cookies __Host-... (Secure; Path=/; no Domain) so no other app's page can shadow them. In hub tabs localStorage persists per app in that browser and sessionStorage until the tab closes, with no cookies or IndexedDB; a page opened on its own keeps storage only for the page. Keep state that matters, or that people share, in the backend."
	}
	if s.PrivateAppOrigin != "" {
		share += " Each app also has a private address, " + s.PrivateAppOrigin + "/ with the same label (get_app private_url), for the workspace's people only: it is where an app installs on a phone as an app of its own, with real browser storage. Point people at open_url, which signs them in there."
	}
	keep := s.Store.KeepVersionsOrDefault()
	retention := fmt.Sprintf("Rollback reaches the newest %d versions that built (get_app's oldest_rollback), plus the live version and versions links pin; older versions lose their files. Deploys share unchanged files with the version before, and a Go backend whose files did not change reuses its build, so versions are cheap.", keep)
	return strings.NewReplacer("{BASE}", base, "{RETENTION}", retention, "{GO}", goLine, "{OS}", info.OS, "{ARCH}", info.Arch, "{RUST}", rustTargetFor(info.OS, info.Arch),
		"{WHERE}", where, "{SANDBOX}", info.Sandbox.Detail, "{NETWORK}", network, "{LIBS}", LibraryNames(), "{SHARE}", share, "{STORAGE}", storage).Replace(appGuideText)
}

func rustTargetFor(goos, arch string) string {
	m := map[string]string{"linux/amd64": "x86_64-unknown-linux-musl", "linux/arm64": "aarch64-unknown-linux-musl", "darwin/arm64": "aarch64-apple-darwin", "darwin/amd64": "x86_64-apple-darwin"}
	if t, ok := m[goos+"/"+arch]; ok {
		return t
	}
	return arch + "-" + goos
}

const appGuideText = `shartifacts apps: full-stack micro frontends. Each app opens as a tab on the home page ({BASE}/), and every deployment is a numbered, immutable version you can preview, promote or roll back.

STACK (fixed)
Frontend: static files served from the app's web root. A page may load a TypeScript/JSX entry Vite-style, <script type="module" src="/src/main.tsx"></script>: on deploy shartifacts bundles it with esbuild (React 19 with the automatic JSX runtime, CSS imports, relative imports, @/ for src/, small images inlined) against the vendored libraries: {LIBS}. Tailwind classes work. For any other npm package, build locally (vite build --base=./) and deploy the output with "frontend": {"root": "dist"}.
Backend (optional): a native HTTP server process.
- Go (recommended): package main in server/ (server/main.go), {GO}. Include go.mod and go.sum; modules download during the build (stdlib-only apps need no network), or vendor them (vendor/).
- Rust or anything else: a prebuilt static executable for {OS}/{ARCH} at server/app (Rust: cargo build --release --target {RUST}; Go elsewhere: CGO_ENABLED=0 GOOS={OS} GOARCH={ARCH}). Upload it in an archive (below) or as binary_files.
Backend contract:
- Listen on 127.0.0.1:$PORT (env HOST and PORT). Serve routes under /api/: shartifacts strips the app's base, so a page's fetch("api/notes") arrives as /api/notes.
- Keep data in $DATA_DIR: it persists across versions and every version shares it (a new version's code meets the old data, so migrate forward). Nothing else is writable. Versions can run at the same time (the live one, a preview, the one just replaced, which stops shortly after), so read data from disk per request rather than caching it in memory, and lock writes (SQLite, or a file lock).
- Log to stdout/stderr (get_app with logs:true shows it). Exit promptly on SIGTERM.
- Env: PORT, HOST, DATA_DIR, APP_ID, APP_VERSION, plus the app's own variables and secrets (manage_app op update, env). Variables change without a deploy; the backend restarts.
- Request headers: X-Shartifacts-User ({"id","name"} of the person using it on the network; absent for public visitors), X-Shartifacts-Access (lan, public or public-read-only), X-Shartifacts-Version, X-Forwarded-Prefix (the base path), X-Forwarded-Host/Proto. Clients cannot forge them.
- One process per version, started on its first request and stopped when idle; the deploy starts it once to prove it listens. WebSockets and server-sent events pass through.
- It runs {WHERE}. {SANDBOX}. Outbound connections need "backend": {"network": true} in shartifacts.json ({NETWORK}).

LAYOUT
index.html              frontend entry (the web root is the tree, or frontend.root)
src/main.tsx, src/*.tsx bundled when a page loads them
public/                 served at the root, like Vite's public folder
server/main.go          Go backend; never served, nor are go.mod, go.sum, vendor/, *.go, *.rs, Cargo.*, shartifacts.json
go.mod, go.sum
shartifacts.json        optional: {"frontend": {"root": "dist", "spa": true}, "backend": {"go": "./server" | "binary": "server/app", "network": false}}
A backend-only app (no index.html) is fine; so is a frontend-only one.

FRONTEND RULES (pages run sandboxed: on the network with an opaque origin, like artifacts)
- Use relative URLs (fetch("api/notes"), <script src="app.js">, url(img/x.png) in CSS). They work at every URL the app is served from: {BASE}/run/ID/live/, version previews {BASE}/run/ID/N/ and share links. Absolute paths (/api/x, /assets/x.js) break. shartifacts adds the page's session to fetch, XHR, EventSource and WebSocket requests under the app's base automatically.
- Route with hashes (#/settings): the sandbox refuses history.pushState to other paths. Unknown extensionless paths still serve index.html.
{STORAGE}
- No external URLs from pages (CDN scripts, fonts, APIs): call outside services from the backend. Media library URLs (/media/...) work on the network.
- window.shartifacts = {app, name, version, live, base, api, public, readOnly, user}.
- Installing: people can install any app on a phone as an app of its own, from its hub tab. Its launcher icon is icon.png at the web root (public/icon.png; a square PNG, 192-2048px), plus icon-maskable.png with the picture inside the middle 80%; without one, a tile of the app's emoji. Lay the page out for a phone-sized window with no browser toolbar.

DEPLOYING
- deploy_app without an id creates the app from name and files (or template: "notes", a working React + Go app to start from); deploy_app with the id ships every change. A deploy builds the version, starts its backend once, and makes it live; promote:false stages it for preview instead. A failed build never goes live: read error and log in the result, or get_app with logs:true. manage_app op promote rolls back or forward to any ready version.
- deploy_app with an id: files are text by path, binary_files base64 (keep them small). merge:true starts from the latest version so you send only what changed: files to add or replace, remove for paths to delete, edits ({file, old_str, new_str}) for small changes. Always pass base_version (the latest version, from get_app).
- Shell agents can upload a zip or tar.gz of the project (one top-level folder is unwrapped):
  curl --fail-with-body -H 'Content-Type: application/zip' --data-binary @app.zip '{BASE}/api/apps/ID/deployments?note=what+changed'
  (new app: POST {BASE}/api/apps?name=My+App). Add &merge=true to overlay the previous version, &promote=false to stage. Up to 256 MiB and 5000 files.
- Test the backend with manage_app op call, or curl {BASE}/run/ID/live/api/... (requests without an Origin header need no app session; once sign-in is on, send your Authorization header, which the backend never sees). get_app with logs:true shows backend output and errors browsers hit in the pages.
- manage_app op share makes a public link (op unshare revokes one): following the live version or pinned to one, optionally read-only (its visitors' writes are refused before they reach the backend). {SHARE}
- {RETENTION}
`

// recentVersions trims an app view's deployments, newest first, to the newest n and any
// older version that is live or a link pins, and counts what it left out. The history grows
// with every deploy and agents rarely need more than its end.
func recentVersions(view map[string]any, a store.App, n int) {
	deps, _ := view["deployments"].([]store.Deployment)
	pinned := map[int]bool{a.Live: true}
	for _, sh := range a.Shares {
		pinned[sh.Version] = true
	}
	kept := []store.Deployment{}
	for i, d := range deps {
		if i < n || pinned[d.Version] {
			kept = append(kept, d)
		}
	}
	if omitted := len(deps) - len(kept); omitted > 0 {
		view["deployments"] = kept
		view["older_versions_omitted"] = omitted
	}
}

// Apps have three tools, grouped by what an agent is doing: get_app reads (the guide, the
// list, an app, its files and logs), deploy_app ships code (creating the app on its first
// deploy), and manage_app operates it (promote, update, restart, call, share, delete).
// base is the address the agent connected through (the guide's curl examples, call); links
// is where the URLs it hands to people point (linkBase).
func (s *Server) addAppTools(m *mcp.Server, base, links string) {
	type GetInput struct {
		ID      string   `json:"id,omitempty"`
		Guide   bool     `json:"guide,omitempty" jsonschema:"Read it before the first deploy"`
		Version int      `json:"version,omitempty" jsonschema:"Version for files and logs; default the latest for files, the live one for logs"`
		Files   []string `json:"files,omitempty"`
		List    bool     `json:"list_files,omitempty"`
		Logs    bool     `json:"logs,omitempty"`
		Tail    int      `json:"tail,omitempty" jsonschema:"With logs: most recent lines (default 200)"`
		All     bool     `json:"all_versions,omitempty" jsonschema:"List every version; by default the newest 10, plus the live one and versions links pin"`
	}
	mcp.AddTool(m, &mcp.Tool{Name: "get_app", Description: "Read apps (full-stack micro frontends: React/TypeScript frontend, Go or prebuilt binary backend). guide:true returns how to build one for this server. No id lists apps and the runtime. With an id: the newest versions (status, notes, errors), live version, how far back rollback reaches, URLs, env names and share links; optionally a version's files, file contents and logs. latest is the base_version deploy_app needs."}, func(ctx context.Context, req *mcp.CallToolRequest, in GetInput) (*mcp.CallToolResult, any, error) {
		if in.ID == "" {
			if in.Guide {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s.appGuide(ctx, base)}}}, nil, nil
			}
			return nil, map[string]any{"apps": s.Store.Apps(), "runtime": s.Runtime.Info(ctx)}, nil
		}
		a, err := s.Store.App(in.ID)
		if err != nil {
			return nil, nil, err
		}
		out := s.appView(a, links, false)
		if !in.All {
			recentVersions(out, a, 10)
		}
		v := in.Version
		if v == 0 {
			v = len(a.Deployments)
		}
		if v > 0 && in.List {
			files, err := s.Store.AppFiles(in.ID, v)
			if err != nil {
				return nil, nil, err
			}
			out["files"] = files
		}
		if v > 0 && len(in.Files) > 0 {
			contents := map[string]string{}
			for _, p := range in.Files {
				b, err := s.Store.ReadAppFile(in.ID, v, p)
				if err != nil {
					return nil, nil, err
				}
				if !utf8.Valid(b) {
					contents[p] = fmt.Sprintf("(binary, %d bytes)", len(b))
					continue
				}
				contents[p] = string(b)
			}
			out["contents"], out["contents_version"] = contents, v
		}
		if in.Logs {
			logs, err := s.appLogs(ctx, in.ID, in.Version, in.Tail)
			if err != nil {
				return nil, nil, err
			}
			out["logs"] = logs
		}
		if in.Guide {
			out["guide"] = s.appGuide(ctx, base)
		}
		return nil, out, nil
	})

	type DeployInput struct {
		ID          string            `json:"id,omitempty" jsonschema:"The app to deploy a new version of; omit to create an app (name required)"`
		Name        string            `json:"name,omitempty"`
		Description string            `json:"description,omitempty"`
		Icon        string            `json:"icon,omitempty" jsonschema:"Creating: one short generic word (chart, list, money, calendar, game, tool) or an emoji"`
		Label       string            `json:"label,omitempty" jsonschema:"Creating: the subdomain of its public links and private address when apps get hostnames of their own (get_app guide), e.g. budget; lowercase letters, digits and hyphens; default a generated one"`
		Template    string            `json:"template,omitempty" jsonschema:"Creating: notes starts from a working React + Go app (files you send replace its files)"`
		BaseVersion int               `json:"base_version,omitempty" jsonschema:"Deploying: the latest version (get_app's latest); a stale one is refused"`
		Merge       bool              `json:"merge,omitempty" jsonschema:"Deploying: start from the latest version and apply files, remove and edits; otherwise the version is exactly files"`
		Files       map[string]string `json:"files,omitempty"`
		BinaryFiles map[string]string `json:"binary_files,omitempty" jsonschema:"Base64 files by path (small images, a prebuilt server/app)"`
		Remove      []string          `json:"remove,omitempty" jsonschema:"With merge: paths (files or directories) to delete"`
		Edits       []store.Edit      `json:"edits,omitempty" jsonschema:"With merge: {file, old_str, new_str} against the latest version, old_str occurring once"`
		Note        string            `json:"note,omitempty"`
		Promote     *bool             `json:"promote,omitempty" jsonschema:"Make it live once it builds (default true); false stages it for preview"`
	}
	mcp.AddTool(m, &mcp.Tool{Name: "deploy_app", Description: "Ship code: without an id, create an app with its first version; with one, deploy a new version. Each version is built, its backend started once, and (by default) made live. Prefer merge with edits or changed files for small changes. Returns the version's status (error and build log on failure) and its URLs."}, func(ctx context.Context, req *mcp.CallToolRequest, in DeployInput) (*mcp.CallToolResult, any, error) {
		di := deployInput{Files: in.Files, BinaryFiles: in.BinaryFiles, Remove: in.Remove, Edits: in.Edits, Merge: in.Merge, Note: in.Note}
		promote := in.Promote == nil || *in.Promote
		if in.ID == "" {
			if in.Merge || len(in.Remove) > 0 || len(in.Edits) > 0 {
				return nil, nil, errors.New("merge, remove and edits change an existing app's latest version; send files (or a template) to create one")
			}
			di.Template = in.Template
			stage, err := di.stage()
			if err != nil {
				return nil, nil, err
			}
			stage.BaseVersion = 0
			if len(stage.Files) == 0 {
				return nil, nil, errors.New("send files or a template; an app starts with its first version")
			}
			a, err := s.createApp(store.AppMeta{Name: &in.Name, Description: &in.Description, Icon: &in.Icon, Label: &in.Label})
			if err != nil {
				return nil, nil, err
			}
			d, a, err := s.deploy(a.ID, stage, true, DeployWait)
			if err != nil {
				s.Store.DeleteApp(a.ID)
				return nil, nil, err
			}
			return nil, s.deployResult(a, d, links), nil
		}
		if in.Template != "" {
			return nil, nil, errors.New("template starts a new app; omit id to create one")
		}
		base0 := in.BaseVersion
		di.BaseVersion = &base0
		stage, err := di.stage()
		if err != nil {
			return nil, nil, err
		}
		d, a, err := s.deploy(in.ID, stage, promote, DeployWait)
		if err != nil {
			return nil, nil, err
		}
		return nil, s.deployResult(a, d, links), nil
	})

	type ManageInput struct {
		ID string `json:"id"`
		Op string `json:"op" jsonschema:"promote (version), update (name, description, icon, pinned, label, env), restart, call (method, path, body, headers, version), share (version, read_only), unshare (token) or delete (confirm)"`
		// promote, call, share
		Version int `json:"version,omitempty" jsonschema:"promote: the ready version to make live (rollback or forward); call: default live; share: a version to pin, or 0 to follow live"`
		// update
		Name        *string            `json:"name,omitempty"`
		Description *string            `json:"description,omitempty"`
		Icon        *string            `json:"icon,omitempty"`
		Pinned      *bool              `json:"pinned,omitempty"`
		Label       *string            `json:"label,omitempty" jsonschema:"update: the subdomain of its public links and private address, e.g. budget (lowercase letters, digits, hyphens; empty for the generated one). Links on its old hostnames redirect, but visitors' browser storage stays there, so set it before sharing"`
		Env         map[string]*string `json:"env,omitempty" jsonschema:"update: backend variables and secrets to set (UPPER_SNAKE) or remove (null); values are never shown again; backends restart"`
		// call
		Method  string            `json:"method,omitempty"`
		Path    string            `json:"path,omitempty" jsonschema:"call: backend path, starting /api/"`
		Body    string            `json:"body,omitempty"`
		Headers map[string]string `json:"headers,omitempty"`
		// share, unshare
		ReadOnly bool   `json:"read_only,omitempty"`
		Token    string `json:"token,omitempty"`
		// delete
		Confirm string `json:"confirm,omitempty" jsonschema:"delete: the token the first call returned, after the person confirmed"`
	}
	destructive := true
	mcp.AddTool(m, &mcp.Tool{Name: "manage_app", Description: "Operate an app by op: promote a version (rollback), update its name, description, icon, pin, public-link subdomain (label) or backend env/secrets, restart its backends, call its backend to test an API, share a public link (following live or pinned, optionally read-only) or unshare one, or delete it (only when the person asks: the first call returns a token to confirm with).", Annotations: &mcp.ToolAnnotations{DestructiveHint: &destructive}}, func(ctx context.Context, req *mcp.CallToolRequest, in ManageInput) (*mcp.CallToolResult, any, error) {
		a, err := s.Store.App(in.ID)
		if err != nil {
			return nil, nil, err
		}
		switch in.Op {
		case "promote":
			if a, err = s.promote(in.ID, in.Version); err != nil {
				return nil, nil, err
			}
			return nil, map[string]any{"id": a.ID, "live": a.Live, "live_url": appURL(links, a.ID, a.Live, true)}, nil
		case "update":
			if a, err = s.updateApp(in.ID, store.AppMeta{Name: in.Name, Description: in.Description, Icon: in.Icon, Pinned: in.Pinned, Label: in.Label}); err != nil {
				return nil, nil, err
			}
			if len(in.Env) > 0 {
				if a, err = s.setAppEnv(ctx, in.ID, in.Env); err != nil {
					return nil, nil, err
				}
			}
			v := s.appView(a, links, false)
			delete(v, "deployments")
			return nil, v, nil
		case "restart":
			if err := s.Runtime.Stop(ctx, in.ID, 0); err != nil {
				return nil, nil, err
			}
			return nil, map[string]any{"restarted": true, "message": "backends stopped; the next request starts them"}, nil
		case "call":
			out, err := s.callApp(ctx, a, in.Version, in.Method, in.Path, in.Body, in.Headers, base)
			return nil, out, err
		case "share":
			sh, err := s.createAppShare(in.ID, in.Version, in.ReadOnly)
			return nil, sh, err
		case "unshare":
			sh, owner, ok := s.Store.AppShared(in.Token)
			if !ok || owner.ID != in.ID {
				return nil, nil, errors.New("no such link on this app")
			}
			revoked, err := s.Store.RevokeAppShare(sh.Token)
			return nil, map[string]bool{"revoked": revoked}, err
		case "delete":
			return s.deleteAppTool(ctx, a, in.Confirm)
		}
		return nil, nil, errors.New("op must be promote, update, restart, call, share, unshare or delete")
	})
}

// callApp sends one request to a version's backend, as manage_app's call op.
func (s *Server) callApp(ctx context.Context, a store.App, version int, method, path, body string, headers map[string]string, base string) (map[string]any, error) {
	v := version
	if v == 0 {
		v = a.Live
	}
	if v < 1 || v > len(a.Deployments) {
		return nil, errors.New("no such version (the app has no live version yet?)")
	}
	if d := a.Deployments[v-1]; d.Backend == "" || d.Status != store.StatusReady {
		return nil, fmt.Errorf("version %d has no running backend (status %s, backend %q)", v, d.Status, d.Backend)
	}
	if !strings.HasPrefix(path, "/api") {
		return nil, errors.New("path must start with /api/")
	}
	method = strings.ToUpper(method)
	if method == "" {
		method = "GET"
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	r := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	r.Host = strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
	for k, val := range headers {
		if !strings.HasPrefix(strings.ToLower(k), "x-shartifacts-") {
			r.Header.Set(k, val)
		}
	}
	if body != "" && r.Header.Get("Content-Type") == "" {
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("X-Forwarded-Prefix", "/run/"+a.ID+"/"+strconv.Itoa(v))
	r.Header.Set("X-Shartifacts-Access", "lan")
	r.Header.Set("X-Shartifacts-Version", strconv.Itoa(v))
	rec := &recorder{ResponseRecorder: httptest.NewRecorder()}
	s.Runtime.Proxy(rec, r, a.ID, v)
	res := rec.Result()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	hs := map[string]string{}
	for k := range res.Header {
		if !slices.Contains([]string{"Date", "Content-Length"}, k) {
			hs[k] = res.Header.Get(k)
		}
	}
	out := map[string]any{"status": res.StatusCode, "headers": hs}
	if utf8.Valid(b) {
		out["body"] = string(b)
	} else {
		out["body"] = fmt.Sprintf("(binary, %d bytes)", len(b))
	}
	if rec.Body.Len() > 64<<10 {
		out["truncated"] = true
	}
	return out, nil
}

// deleteAppTool deletes an app in two calls: the first returns a token for the person to
// confirm, the second (with it) deletes.
func (s *Server) deleteAppTool(ctx context.Context, a store.App, confirm string) (*mcp.CallToolResult, any, error) {
	appDeleteMu.Lock()
	defer appDeleteMu.Unlock()
	for k, p := range appDeleteTokens {
		if time.Now().After(p.expires) {
			delete(appDeleteTokens, k)
		}
	}
	if confirm == "" {
		b := make([]byte, 8)
		rand.Read(b)
		token := hex.EncodeToString(b)
		appDeleteTokens[token] = pendingDelete{id: a.ID, expires: time.Now().Add(10 * time.Minute)}
		return nil, map[string]any{"confirm": token, "name": a.Name, "versions": len(a.Deployments), "message": fmt.Sprintf("Ask the person to confirm deleting the app %q, its %d versions and all its backend data; it cannot be undone. Then call again with op delete and this confirm token (valid 10 minutes).", a.Name, len(a.Deployments))}, nil
	}
	p, ok := appDeleteTokens[confirm]
	if !ok || p.id != a.ID {
		return nil, nil, errors.New("that confirmation is unknown or expired; call without confirm to ask again")
	}
	delete(appDeleteTokens, confirm)
	s.Runtime.Stop(ctx, a.ID, 0)
	if err := s.Store.DeleteApp(a.ID); err != nil {
		return nil, nil, err
	}
	return nil, map[string]any{"deleted": true, "id": a.ID}, nil
}

var (
	appDeleteMu     sync.Mutex
	appDeleteTokens = map[string]pendingDelete{}
)

// recorder lets the backend's streaming flushes through httptest's recorder.
type recorder struct{ *httptest.ResponseRecorder }

func (r *recorder) Flush() {}

// deployResult is what an agent needs back from a deploy: the outcome without the
// source it sent, the log only when something went wrong.
func (s *Server) deployResult(a store.App, d store.Deployment, base string) map[string]any {
	out := map[string]any{"id": a.ID, "name": a.Name, "version": d.Version, "status": d.Status, "live": a.Live, "latest": len(a.Deployments),
		"hub_url": base + "/apps/" + a.ID, "preview_url": appURL(base, a.ID, d.Version, false)}
	if d.Backend != "" {
		out["backend"] = d.Backend
	}
	if a.Live > 0 {
		out["live_url"] = appURL(base, a.ID, a.Live, true)
	}
	if len(d.Warnings) > 0 {
		out["warnings"] = d.Warnings
	}
	switch d.Status {
	case store.StatusFailed:
		out["error"], out["log"] = d.Error, d.Log
	case store.StatusBuilding:
		out["message"] = "still building; check get_app (logs:true) for the outcome"
	}
	return out
}
