package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/LaneBirmingham/shartifacts/internal/runner"
	"github.com/LaneBirmingham/shartifacts/internal/store"
	"github.com/evanw/esbuild/pkg/api"
)

// appManifest is shartifacts.json, an app's optional build settings. Without one an app
// follows the conventions the guide (get_app guide:true) describes.
type appManifest struct {
	Frontend struct {
		// Root is the web root inside the source tree ("dist" for a prebuilt frontend).
		Root string `json:"root,omitempty"`
		// SPA serves index.html for unknown paths without an extension (default true).
		SPA *bool `json:"spa,omitempty"`
	} `json:"frontend"`
	Backend struct {
		// Go is the package to build ("./server" by default when server/ holds Go files).
		Go string `json:"go,omitempty"`
		// Binary is a prebuilt executable (Rust, or Go built elsewhere).
		Binary string `json:"binary,omitempty"`
		// Network lets the backend open outbound connections.
		Network bool `json:"network,omitempty"`
	} `json:"backend"`
}

// appSite is out/site.json: how a built version serves its frontend.
type appSite struct {
	Root     string `json:"root"`
	SPA      bool   `json:"spa"`
	Frontend bool   `json:"frontend"`
	Binary   string `json:"binary,omitempty"`
	// Backend is the Go backend's package directory when it is not server/, which is
	// backend material as server/ is.
	Backend string `json:"backend,omitempty"`
}

const manifestName = "shartifacts.json"

func readManifest(src string) (appManifest, error) {
	var m appManifest
	b, err := os.ReadFile(filepath.Join(src, manifestName))
	if os.IsNotExist(err) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(&m); err != nil {
		return m, fmt.Errorf("%s: %v (fields: frontend.root, frontend.spa, backend.go, backend.binary, backend.network)", manifestName, err)
	}
	if m.Frontend.Root != "" {
		root := strings.Trim(path.Clean(m.Frontend.Root), "/")
		if root == "." {
			root = ""
		}
		if root != "" && store.ValidAppPath(root) != nil {
			return m, fmt.Errorf("%s: frontend.root must be a directory inside the app, like dist", manifestName)
		}
		m.Frontend.Root = root
	}
	if m.Backend.Go != "" && m.Backend.Binary != "" {
		return m, fmt.Errorf("%s: set backend.go or backend.binary, not both", manifestName)
	}
	if m.Backend.Binary != "" && store.ValidAppPath(m.Backend.Binary) != nil {
		return m, fmt.Errorf("%s: backend.binary must be a path inside the app, like server/app", manifestName)
	}
	return m, nil
}

// serverOnly reports whether a source path is backend material, never served to browsers.
func serverOnly(p string, site appSite) bool {
	top, _, _ := strings.Cut(p, "/")
	binary, backend := site.Binary, path.Clean(site.Backend)
	switch {
	case site.Backend != "" && backend != "." && (p == backend || strings.HasPrefix(p, backend+"/")):
		return true
	case top == "server" || top == "vendor" || top == "target":
		return true
	case p == "go.mod" || p == "go.sum" || p == "go.work" || p == "go.work.sum" || p == manifestName || p == "Cargo.toml" || p == "Cargo.lock":
		return true
	case strings.HasSuffix(p, ".go") || strings.HasSuffix(p, ".rs"):
		return true
	case binary != "" && p == binary:
		return true
	}
	return false
}

// buildDeployment builds a staged version: its frontend here, its backend through the
// runtime, then starts the backend once to prove it serves. It returns the deployment as
// it should be recorded.
func (s *Server) buildDeployment(ctx context.Context, id string, d store.Deployment) store.Deployment {
	d.Status, d.Warnings, d.Error, d.Log = store.StatusFailed, nil, "", ""
	src, out := s.Store.AppSourceDir(id, d.Version), s.Store.AppBuildDir(id, d.Version)
	var log strings.Builder
	fail := func(msg string) store.Deployment {
		d.Error = msg
		d.Log = log.String()
		return d
	}
	os.RemoveAll(out)
	if err := os.MkdirAll(filepath.Join(out, "public"), 0700); err != nil {
		return fail(err.Error())
	}
	m, err := readManifest(src)
	if err != nil {
		return fail(err.Error())
	}
	site := appSite{Root: m.Frontend.Root, SPA: m.Frontend.SPA == nil || *m.Frontend.SPA, Binary: m.Backend.Binary, Backend: m.Backend.Go}
	rootDir := filepath.Join(src, filepath.FromSlash(site.Root))
	if info, err := os.Stat(filepath.Join(rootDir, "index.html")); err == nil && !info.IsDir() {
		site.Frontend = true
	}
	if site.Root != "" && !site.Frontend {
		return fail(fmt.Sprintf("frontend.root is %q but %s/index.html does not exist", site.Root, site.Root))
	}
	if site.Frontend {
		built, warnings, err := buildFrontend(src, site, filepath.Join(out, "public"))
		d.Warnings = append(d.Warnings, warnings...)
		if err != nil {
			return fail(err.Error())
		}
		if built > 0 {
			fmt.Fprintf(&log, "frontend: bundled %d page entr%s with esbuild\n", built, map[bool]string{true: "y", false: "ies"}[built == 1])
		} else {
			log.WriteString("frontend: static files, served as deployed\n")
		}
	}
	req := runner.BuildRequest{App: id, Version: d.Version}
	switch {
	case m.Backend.Binary != "":
		req.Kind, req.Binary = "binary", m.Backend.Binary
	case m.Backend.Go != "":
		req.Kind, req.Package = "go", m.Backend.Go
	default:
		if goFiles, _ := filepath.Glob(filepath.Join(src, "server", "*.go")); len(goFiles) > 0 {
			req.Kind, req.Package = "go", "./server"
		} else if info, err := os.Stat(filepath.Join(src, "server", "app")); err == nil && !info.IsDir() {
			req.Kind, req.Binary = "binary", "server/app"
		}
	}
	if !site.Frontend && req.Kind == "" {
		return fail("nothing to serve: add index.html for a frontend and/or server/main.go (Go) or server/app (prebuilt binary) for a backend; get_app with guide:true explains the layout")
	}
	siteJSON, _ := json.Marshal(site)
	rt, _ := json.Marshal(runner.RuntimeConfig{Network: m.Backend.Network})
	if err := os.WriteFile(filepath.Join(out, "site.json"), siteJSON, 0600); err != nil {
		return fail(err.Error())
	}
	if err := os.WriteFile(filepath.Join(out, "runtime.json"), rt, 0600); err != nil {
		return fail(err.Error())
	}
	d.Frontend, d.Network = site.Frontend, m.Backend.Network && req.Kind != ""
	if req.Kind != "" {
		res := s.Runtime.Build(ctx, req)
		log.WriteString(res.Log)
		d.Warnings = append(d.Warnings, res.Warnings...)
		if !res.OK {
			return fail(res.Error)
		}
		d.Backend = req.Kind
		if err := s.Runtime.Start(ctx, id, d.Version); err != nil {
			s.Runtime.Stop(context.Background(), id, d.Version)
			return fail("the backend built but did not start: " + err.Error())
		}
		log.WriteString("backend: started and listening\n")
	}
	d.Status = store.StatusReady
	d.Log = log.String()
	return d
}

// moduleScript finds script tags whose src may be a page entry to bundle.
var (
	appScriptTag = regexp.MustCompile(`(?is)<script\b([^>]*)>\s*</script>`)
	srcAttr      = regexp.MustCompile(`(?i)\bsrc\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	moduleAttr   = regexp.MustCompile(`(?i)\btype\s*=\s*["']?module\b`)
	headClose    = regexp.MustCompile(`(?i)</head\s*>`)
)

// buildFrontend bundles the TypeScript/JSX entries pages load (a Vite-style
// <script type="module" src="/src/main.tsx">) with esbuild against the vendored libraries,
// and writes each changed page and the bundles to pub. It returns how many entries it built.
func buildFrontend(src string, site appSite, pub string) (int, []string, error) {
	rootDir := filepath.Join(src, filepath.FromSlash(site.Root))
	type page struct {
		rel  string
		html string
	}
	var pages []page
	tailwind := false
	err := filepath.WalkDir(rootDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(rootDir, p)
		rel = filepath.ToSlash(rel)
		treeRel := path.Join(site.Root, rel)
		if d.IsDir() {
			if rel != "." && (serverOnly(treeRel, site) || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		switch path.Ext(rel) {
		case ".html", ".htm":
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			pages = append(pages, page{rel, string(b)})
			tailwind = tailwind || strings.Contains(string(b), "cdn.tailwindcss.com")
		case ".tsx", ".jsx":
			if b, err := os.ReadFile(p); err == nil && usesTailwind(string(b)) {
				tailwind = true
			}
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	built := map[string]bundle{}
	var warnings []string
	for _, pg := range pages {
		dir := path.Dir(pg.rel)
		changed := false
		var buildErr error
		html := appScriptTag.ReplaceAllStringFunc(pg.html, func(tag string) string {
			attrs := appScriptTag.FindStringSubmatch(tag)[1]
			m := srcAttr.FindStringSubmatch(attrs)
			if m == nil || !moduleAttr.MatchString(attrs) || buildErr != nil {
				return tag
			}
			ref := m[1] + m[2]
			if strings.Contains(ref, "://") || strings.HasPrefix(ref, "//") {
				return tag
			}
			ref, _, _ = strings.Cut(ref, "?")
			ref, _, _ = strings.Cut(ref, "#")
			entry := path.Clean(path.Join(dir, ref))
			if strings.HasPrefix(ref, "/") {
				entry = path.Clean(strings.TrimPrefix(ref, "/"))
			}
			if !slices.Contains([]string{".ts", ".tsx", ".jsx"}, path.Ext(entry)) || strings.HasPrefix(entry, "../") {
				return tag
			}
			b, ok := built[entry]
			if !ok {
				if _, err := os.Stat(filepath.Join(rootDir, filepath.FromSlash(entry))); err != nil {
					buildErr = fmt.Errorf("%s loads %s, which is not in the app", pg.rel, ref)
					return tag
				}
				b, buildErr = bundleEntry(rootDir, entry, pub)
				if buildErr != nil {
					return tag
				}
				built[entry] = b
			}
			changed = true
			return b.tags(relTo(dir, b.js), relTo(dir, b.css))
		})
		if buildErr != nil {
			return 0, warnings, buildErr
		}
		if !changed {
			continue
		}
		head := ""
		if tailwind || slices.ContainsFunc(slices.Collect(mapValues(built)), func(b bundle) bool { return slices.Contains(b.libs, "ui") }) {
			head = `<script src="/vendor/lib/tailwind.js"></script>` + shadcnTheme
		}
		if head != "" {
			if loc := headClose.FindStringIndex(html); loc != nil {
				html = html[:loc[0]] + head + html[loc[0]:]
			} else {
				html = head + html
			}
		}
		if err := writeFileAll(filepath.Join(pub, filepath.FromSlash(pg.rel)), []byte(html)); err != nil {
			return 0, warnings, err
		}
	}
	return len(built), warnings, nil
}

func mapValues[K comparable, V any](m map[K]V) func(func(V) bool) {
	return func(yield func(V) bool) {
		for _, v := range m {
			if !yield(v) {
				return
			}
		}
	}
}

// relTo is target (a web-root path) relative to the page directory dir.
func relTo(dir, target string) string {
	if target == "" {
		return ""
	}
	if dir == "." {
		return target
	}
	rel, err := filepath.Rel(filepath.FromSlash(dir), filepath.FromSlash(target))
	if err != nil {
		return target
	}
	return filepath.ToSlash(rel)
}

type bundle struct {
	js, css string
	libs    []string
}

// tags load the vendored React and libraries the bundle imports, then the bundle, in order.
func (b bundle) tags(js, css string) string {
	var out strings.Builder
	if css != "" {
		fmt.Fprintf(&out, `<link rel="stylesheet" href="%s">`, css)
	}
	out.WriteString(`<script defer src="/vendor/react.js"></script>`)
	for _, lib := range b.libs {
		fmt.Fprintf(&out, `<script defer src="/vendor/lib/%s.js"></script>`, lib)
	}
	fmt.Fprintf(&out, `<script defer src="%s"></script>`, js)
	return out.String()
}

// bundleEntry builds one entry (a web-root path) into pub/_build.
func bundleEntry(rootDir, entry, pub string) (bundle, error) {
	var mu sync.Mutex
	used := map[string]bool{}
	assets := map[string]api.Loader{}
	for _, ext := range []string{".png", ".jpg", ".jpeg", ".gif", ".webp", ".avif", ".svg", ".ico", ".woff", ".woff2", ".ttf", ".otf", ".mp3", ".wav", ".mp4", ".webm", ".wasm"} {
		assets[ext] = api.LoaderDataURL
	}
	for _, ext := range []string{".txt", ".md", ".csv", ".html"} {
		assets[ext] = api.LoaderText
	}
	result := api.Build(api.BuildOptions{
		EntryPoints:      []string{filepath.Join(rootDir, filepath.FromSlash(entry))},
		AbsWorkingDir:    rootDir,
		Bundle:           true,
		Write:            false,
		Outdir:           filepath.Join(pub, "_build"),
		EntryNames:       "[name]-[hash]",
		Format:           api.FormatIIFE,
		Platform:         api.PlatformBrowser,
		Target:           api.ES2020,
		JSX:              api.JSXAutomatic,
		Loader:           assets,
		Sourcemap:        api.SourceMapLinked,
		MinifyWhitespace: true, MinifyIdentifiers: true, MinifySyntax: true,
		Define:   map[string]string{"process.env.NODE_ENV": `"production"`, "import.meta.env.MODE": `"production"`, "import.meta.env.PROD": "true", "import.meta.env.DEV": "false", "import.meta.env.BASE_URL": `"./"`},
		LogLevel: api.LogLevelSilent,
		Plugins: []api.Plugin{{Name: "vendored", Setup: func(b api.PluginBuild) {
			b.OnResolve(api.OnResolveOptions{Filter: `^[^./]`}, func(a api.OnResolveArgs) (api.OnResolveResult, error) {
				if strings.HasPrefix(a.Path, "node:") {
					return api.OnResolveResult{}, fmt.Errorf("%s is a server module; browser code cannot import it", a.Path)
				}
				if _, ok := reactModules[a.Path]; ok {
					return api.OnResolveResult{Path: a.Path, Namespace: "react"}, nil
				}
				// Vite's @/ alias for src/, before shadcn/ui's @/components/ui paths.
				if rest, ok := strings.CutPrefix(a.Path, "@/"); ok {
					if found := resolveDisk(filepath.Join(rootDir, "src", filepath.FromSlash(rest))); found != "" {
						return api.OnResolveResult{Path: found}, nil
					}
				}
				if _, _, ok := libraryFor(a.Path); ok {
					return api.OnResolveResult{Path: a.Path, Namespace: "lib"}, nil
				}
				return api.OnResolveResult{}, nil // node_modules shipped with the app, if any
			})
			b.OnLoad(api.OnLoadOptions{Filter: ".*", Namespace: "react"}, func(a api.OnLoadArgs) (api.OnLoadResult, error) {
				src := "module.exports=window." + reactModules[a.Path] + ";"
				return api.OnLoadResult{Contents: &src, Loader: api.LoaderJS}, nil
			})
			b.OnLoad(api.OnLoadOptions{Filter: ".*", Namespace: "lib"}, func(a api.OnLoadArgs) (api.OnLoadResult, error) {
				lib, member, _ := libraryFor(a.Path)
				mu.Lock()
				used[lib] = true
				mu.Unlock()
				src := fmt.Sprintf("module.exports=window.__shartifactsLib[%q]%s;", lib, member)
				return api.OnLoadResult{Contents: &src, Loader: api.LoaderJS}, nil
			})
		}}},
	})
	if len(result.Errors) > 0 {
		var msgs []string
		for i, e := range result.Errors {
			if i == 5 {
				msgs = append(msgs, fmt.Sprintf("… and %d more", len(result.Errors)-5))
				break
			}
			msg := e.Text
			if e.Location != nil {
				file := e.Location.File
				if rel, err := filepath.Rel(rootDir, file); err == nil {
					file = filepath.ToSlash(rel)
				}
				msg = fmt.Sprintf("%s:%d:%d: %s", file, e.Location.Line, e.Location.Column, e.Text)
			}
			if strings.HasPrefix(e.Text, "Could not resolve \"") {
				msg += " — available without bundling: " + LibraryNames() + ". Bundle other packages yourself and deploy the built files (frontend.root)"
			}
			msgs = append(msgs, msg)
		}
		return bundle{}, errors.New("frontend build failed:\n" + strings.Join(msgs, "\n"))
	}
	var out bundle
	for lib := range used {
		out.libs = append(out.libs, lib)
	}
	sort.Strings(out.libs)
	for _, f := range result.OutputFiles {
		rel, err := filepath.Rel(pub, f.Path)
		if err != nil {
			return bundle{}, err
		}
		rel = filepath.ToSlash(rel)
		switch path.Ext(rel) {
		case ".js":
			out.js = rel
		case ".css":
			out.css = rel
		}
		if err := writeFileAll(f.Path, f.Contents); err != nil {
			return bundle{}, err
		}
	}
	return out, nil
}

// resolveDisk finds an import target on disk, trying the extensions an import may omit.
func resolveDisk(p string) string {
	for _, suffix := range []string{"", ".tsx", ".ts", ".jsx", ".js", ".json", ".css", "/index.tsx", "/index.ts", "/index.jsx", "/index.js"} {
		if info, err := os.Stat(p + suffix); err == nil && !info.IsDir() {
			return p + suffix
		}
	}
	return ""
}

func writeFileAll(p string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	return os.WriteFile(p, b, 0600)
}
