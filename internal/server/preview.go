package server

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"maps"
	"mime"
	"net/http"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/LaneBirmingham/shartifacts/internal/store"
	"github.com/evanw/esbuild/pkg/api"
)

type compiled struct {
	code, css string
	libs      []string
	err       error
}

// reactCache keeps recent builds by content, so reloading a page, or a share link, does not
// run esbuild again.
var reactCache = struct {
	sync.Mutex
	m     map[[32]byte]compiled
	order [][32]byte
}{m: map[[32]byte]compiled{}}

// compileReact bundles an artifact against the vendored libraries and its own files. It
// returns the code, any CSS its files import, and the library bundles the page must load
// first.
func compileReact(source string, files map[string]string) (code, css string, libs []string, err error) {
	h := sha256.New()
	io.WriteString(h, source)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		fmt.Fprintf(h, "\x00%s\x00%d\x00%s", name, len(files[name]), files[name])
	}
	var key [32]byte
	h.Sum(key[:0])
	reactCache.Lock()
	c, ok := reactCache.m[key]
	reactCache.Unlock()
	if !ok {
		c.code, c.css, c.libs, c.err = buildReact(source, files)
		reactCache.Lock()
		if _, ok := reactCache.m[key]; !ok {
			reactCache.m[key] = c
			reactCache.order = append(reactCache.order, key)
			if len(reactCache.order) > 64 {
				delete(reactCache.m, reactCache.order[0])
				reactCache.order = reactCache.order[1:]
			}
		}
		reactCache.Unlock()
	}
	return c.code, c.css, slices.Clone(c.libs), c.err
}

func buildReact(source string, files map[string]string) (code, css string, libs []string, err error) {
	var mu sync.Mutex
	used := map[string]bool{}
	result := api.Build(api.BuildOptions{Stdin: &api.StdinOptions{Contents: source, Loader: api.LoaderTSX, Sourcefile: "artifact.tsx"}, Bundle: true, Write: false, Outfile: "artifact.js", Format: api.FormatIIFE, GlobalName: "ArtifactModule", JSX: api.JSXTransform, JSXFactory: "React.createElement", JSXFragment: "React.Fragment", LogLevel: api.LogLevelSilent, Plugins: []api.Plugin{{Name: "vendored", Setup: func(b api.PluginBuild) {
		b.OnResolve(api.OnResolveOptions{Filter: ".*"}, func(a api.OnResolveArgs) (api.OnResolveResult, error) {
			if strings.HasPrefix(a.Path, "./") || strings.HasPrefix(a.Path, "../") {
				dir := ""
				if a.Namespace == "files" {
					dir = path.Dir(a.Importer)
				}
				if found := resolveFile(files, path.Join(dir, a.Path)); found != "" {
					return api.OnResolveResult{Path: found, Namespace: "files"}, nil
				}
				return api.OnResolveResult{}, fmt.Errorf("file %q is not among the artifact's files", strings.TrimPrefix(path.Join(dir, a.Path), "./"))
			}
			if _, ok := reactModules[a.Path]; ok {
				return api.OnResolveResult{Path: a.Path, Namespace: "react"}, nil
			}
			if _, _, ok := libraryFor(a.Path); ok {
				return api.OnResolveResult{Path: a.Path, Namespace: "lib"}, nil
			}
			return api.OnResolveResult{}, fmt.Errorf("package %q is unavailable; available: %s", a.Path, LibraryNames())
		})
		b.OnLoad(api.OnLoadOptions{Filter: ".*", Namespace: "files"}, func(a api.OnLoadArgs) (api.OnLoadResult, error) {
			src := files[a.Path]
			return api.OnLoadResult{Contents: &src, Loader: fileLoader(a.Path)}, nil
		})
		// CommonJS shims let esbuild's interop give default, named and namespace imports.
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
	}}}})
	if len(result.Errors) > 0 {
		e := result.Errors[0]
		if e.Location != nil && e.Location.File != "artifact.tsx" {
			return "", "", nil, fmt.Errorf("%s: %s", strings.TrimPrefix(e.Location.File, "files:"), e.Text)
		}
		return "", "", nil, fmt.Errorf("%s", e.Text)
	}
	for lib := range used {
		libs = append(libs, lib)
	}
	sort.Strings(libs)
	for _, out := range result.OutputFiles {
		if strings.HasSuffix(out.Path, ".css") {
			css = string(out.Contents)
		} else {
			code = string(out.Contents)
		}
	}
	return code, css, libs, nil
}

// resolveFile finds an import among the files, trying the extensions an import may omit.
func resolveFile(files map[string]string, p string) string {
	p = path.Clean(p)
	if strings.HasPrefix(p, "../") || p == ".." {
		return ""
	}
	for _, suffix := range []string{"", ".tsx", ".ts", ".jsx", ".js", ".json", ".css", "/index.tsx", "/index.ts", "/index.jsx", "/index.js"} {
		if _, ok := files[p+suffix]; ok {
			return p + suffix
		}
	}
	return ""
}

func fileLoader(p string) api.Loader {
	switch path.Ext(p) {
	case ".tsx":
		return api.LoaderTSX
	case ".ts", ".mts":
		return api.LoaderTS
	case ".jsx", ".js", ".mjs":
		return api.LoaderJSX
	case ".json":
		return api.LoaderJSON
	case ".css":
		return api.LoaderCSS
	}
	return api.LoaderText
}

func (s *Server) preview(w http.ResponseWriter, r *http.Request) {
	rev, e := s.revision(r)
	if e != nil {
		respond(w, 404, nil, e)
		return
	}
	// A page with files lives in a directory, so its relative references resolve.
	if len(rev.Files) > 0 {
		http.Redirect(w, r, fmt.Sprintf("/preview/%s/%d/", r.PathValue("id"), rev.Version), http.StatusFound)
		return
	}
	s.renderPreview(w, r, rev, "", "")
}

// previewFile serves a multi-file artifact: its page at the version's directory, and its
// files beneath it. Other HTML files are pages too, rendered in the same sandbox.
func (s *Server) previewFile(w http.ResponseWriter, r *http.Request) {
	a, e := s.Store.Get(r.PathValue("id"))
	v, _ := strconv.Atoi(r.PathValue("version"))
	if e != nil || v < 1 || v > len(a.Revisions) {
		http.NotFound(w, r)
		return
	}
	s.serveArtifactPath(w, r, a.Revisions[v-1], fmt.Sprintf("/preview/%s/%d/", a.ID, v), "")
}
func (s *Server) serveArtifactPath(w http.ResponseWriter, r *http.Request, rev store.Revision, base, objects string) {
	p := r.PathValue("path")
	if p == "" {
		s.renderPreview(w, r, rev, base, objects)
		return
	}
	body, ok := rev.Files[p]
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch path.Ext(p) {
	case ".html", ".htm":
		s.renderPreview(w, r, store.Revision{Title: rev.Title, Kind: "html", Content: body, Files: rev.Files}, base, objects)
		return
	}
	t := mime.TypeByExtension(path.Ext(p))
	if t == "" || strings.HasPrefix(t, "text/html") {
		t = "text/plain; charset=utf-8"
	}
	w.Header().Set("Content-Type", t)
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	// Module scripts and fetches from the opaque-origin preview are CORS requests.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	io.WriteString(w, body)
}

// renderPreview writes a sandboxed page. base, when set, is the URL directory holding the
// artifact's files, which the page may load and fetch. objects is the path under which the
// page reaches media and assets: "" for /media/ and /_blob/, or a share link's /s/{token}.
func (s *Server) renderPreview(w http.ResponseWriter, r *http.Request, rev store.Revision, base, objects string) {
	origin := s.clientScheme(r) + "://" + r.Host
	own := ""
	if base != "" {
		own = " " + origin + base
	}
	// Assets (/_blob/) load as images, media, fonts, styles and scripts and can be fetched as
	// data; they are the artifact's own uploads, read-only.
	blob := " " + origin + objects + "/_blob/"
	csp := fmt.Sprintf("default-src 'none'; script-src 'unsafe-inline' 'unsafe-eval' %[1]s/vendor/ %[1]s/runtime.js%[2]s%[3]s; style-src 'unsafe-inline'%[2]s%[3]s; img-src data: blob: %[4]s/media/%[2]s%[3]s; media-src data: blob: %[4]s/media/%[2]s%[3]s; font-src data: %[1]s/vendor/fonts/%[2]s%[3]s; worker-src blob:; connect-src%[2]s%[3]s; object-src 'none'; base-uri 'none'; form-action 'none'; frame-src 'none'; sandbox "+previewSandbox, origin, own, blob, origin+objects)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", csp)
	// This header keeps a standalone preview in the same opaque-origin sandbox as an iframe.
	fmt.Fprint(w, `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><style>body{margin:0;color:#242922;background:#fff;font:16px/1.6 Georgia,serif}#document{max-width:860px;padding:32px;margin:auto}pre{white-space:pre-wrap;overflow-wrap:anywhere;background:#f4f5f1;padding:18px;border-radius:8px}table{border-collapse:collapse}td,th{border:1px solid #ddd;padding:8px}img,svg,video{max-width:100%}#error{color:#a12a24;padding:20px;font:14px/1.5 monospace;white-space:pre-wrap}</style>`+documentStyle(rev.Kind)+previewRuntime+`<script src="`+html.EscapeString(origin)+`/runtime.js"></script></head><body>`)
	data, _ := json.Marshal(rev.Content)
	switch rev.Kind {
	case "html":
		fmt.Fprint(w, localizeScripts(rev.Content, origin))
	case "svg":
		fmt.Fprint(w, rev.Content)
	case "react":
		code, css, libs, e := compileReact(rev.Content, rev.Files)
		if e != nil {
			fmt.Fprint(w, `<pre id="error">`+html.EscapeString(e.Error())+`</pre>`)
			break
		}
		if css != "" {
			fmt.Fprint(w, "<style>"+strings.ReplaceAll(css, "</style", "<\\/style")+"</style>")
		}
		if slices.Contains(libs, "ui") || usesTailwind(rev.Content) || slices.ContainsFunc(slices.Collect(maps.Values(rev.Files)), usesTailwind) {
			libs = append([]string{"tailwind"}, libs...)
			fmt.Fprint(w, shadcnTheme)
		}
		fmt.Fprintf(w, `<div id="root"></div><script src="%s/vendor/react.js" crossorigin="anonymous"></script>`, html.EscapeString(origin))
		for _, lib := range libs {
			fmt.Fprintf(w, `<script src="%s/vendor/lib/%s.js" crossorigin="anonymous"></script>`, html.EscapeString(origin), lib)
		}
		fmt.Fprintf(w, `<script>%s
 if(!ArtifactModule.default)throw new Error('React artifacts must default-export a component');window.createRoot(document.getElementById('root')).render(React.createElement(ArtifactModule.default));</script>`, strings.ReplaceAll(code, "</script", "<\\/script"))
	case "markdown", "mermaid", "slides":
		fmt.Fprintf(w, `<script src="%s/vendor/documents.js" crossorigin="anonymous"></script><script src="%s/vendor/lib/highlight.js" crossorigin="anonymous"></script>`, html.EscapeString(origin), html.EscapeString(origin))
		if rev.Kind != "mermaid" && hasMath.MatchString(rev.Content) {
			fmt.Fprintf(w, `<script src="%s/vendor/lib/katex.js" crossorigin="anonymous"></script><script>marked.use(window.__shartifactsMath)</script>`, html.EscapeString(origin))
		}
		// render turns fenced mermaid blocks into diagrams and highlights other code.
		fmt.Fprintf(w, `<script>const source=%s;function render(root){root.querySelectorAll('code.language-mermaid').forEach(el=>{const p=document.createElement('pre');p.className='mermaid';p.textContent=el.textContent;el.parentNode.replaceWith(p)});root.querySelectorAll('pre code[class*="language-"]').forEach(el=>{const l=el.className.match(/language-(\S+)/)[1];if(hljs.getLanguage(l))hljs.highlightElement(el)})}</script>`, data)
		switch rev.Kind {
		case "markdown":
			fmt.Fprint(w, `<main id="document"></main><script>document.getElementById('document').innerHTML=marked.parse(source);render(document.getElementById('document'))</script>`)
		case "mermaid":
			fmt.Fprint(w, `<main id="document"></main><script>{const p=document.createElement('pre');p.className='mermaid';p.textContent=source;document.getElementById('document').append(p)}</script>`)
		case "slides":
			fmt.Fprint(w, slidesDeck)
		}
		// Diagrams lay out while every slide is measurable; the deck then shows one at a time.
		fmt.Fprint(w, `<script>mermaid.initialize({startOnLoad:false,securityLevel:'strict',theme:'neutral'});const deckEl=document.getElementById('deck');deckEl&&deckEl.classList.add('measuring');mermaid.run().catch(e=>{(document.getElementById('document')||document.querySelector('.slide.active')||document.body).append(document.createTextNode(String(e)))}).finally(()=>deckEl&&deckEl.classList.remove('measuring'))</script>`)
	default:
		// The title's extension names the language when it has one (server.py, app.tsx).
		lang, _ := json.Marshal(strings.ToLower(strings.TrimPrefix(path.Ext(rev.Title), ".")))
		fmt.Fprintf(w, `<main id="document"><pre><code id="code">%s</code></pre></main><script src="%s/vendor/lib/highlight.js" crossorigin="anonymous"></script><script>{const el=document.getElementById('code'),l=%s;if(hljs.getLanguage(l))el.className='language-'+l;if(l&&hljs.getLanguage(l)||el.textContent.length<200000)hljs.highlightElement(el)}</script>`, html.EscapeString(rev.Content), html.EscapeString(origin), lang)
	}
	readyURL, _ := json.Marshal(r.URL.RequestURI())
	fmt.Fprintf(w, `<script>addEventListener('load',()=>parent.postMessage({type:'shartifacts-preview-ready',url:%s},'*'));</script></body></html>`, readyURL)
}

// designFonts declares the design system's open fonts, served from /vendor/fonts and
// allowed by every preview's font-src.
const designFonts = `@font-face{font-family:Inter;src:url(/vendor/fonts/inter.woff2) format("woff2");font-weight:100 900;font-display:swap}@font-face{font-family:"Cormorant Garamond";src:url(/vendor/fonts/cormorant-garamond-500.woff2) format("woff2");font-weight:400 600;font-display:swap}@font-face{font-family:"Cormorant Garamond";src:url(/vendor/fonts/cormorant-garamond-500-italic.woff2) format("woff2");font-weight:400 600;font-style:italic;font-display:swap}@font-face{font-family:"JetBrains Mono";src:url(/vendor/fonts/jetbrains-mono.woff2) format("woff2");font-weight:100 800;font-display:swap}`

// documentStyle dresses the pages shartifacts renders itself (Markdown, slides, diagrams,
// code) in the design system; HTML, SVG and React artifacts keep only their own styles.
func documentStyle(kind string) string {
	switch kind {
	case "html", "svg", "react":
		return ""
	}
	return `<style>` + designFonts + `body{color:#3d3d3a;background:#faf9f5;font:16px/1.55 StyreneB,Inter,-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif}#document{max-width:760px;padding:48px 32px}h1,h2,h3,h4{font-family:Copernicus,"Tiempos Headline","Cormorant Garamond","EB Garamond",Garamond,"Times New Roman",serif;font-weight:500;letter-spacing:-.02em;color:#141413}h1{font-size:48px;line-height:1.1}h2{font-size:36px;line-height:1.15}h3{font-size:28px;line-height:1.2}h4{font-size:22px;line-height:1.3}strong{color:#252523;font-weight:500}a{color:#cc785c}hr{border:0;border-top:1px solid #e6dfd8;margin:48px 0}blockquote{margin:24px 0;padding-left:24px;border-left:3px solid #e6dfd8;color:#6c6a64}code{font-family:"JetBrains Mono",ui-monospace,SFMono-Regular,Consolas,monospace;font-size:.875em}:not(pre)>code{background:#efe9de;padding:2px 6px;border-radius:4px}pre{background:#181715;color:#faf9f5;padding:24px;border-radius:12px;font-size:14px;line-height:1.6;white-space:pre;overflow-x:auto}pre code.hljs{background:none;color:inherit;padding:0}pre.mermaid{background:none;color:inherit;text-align:center}table{font-size:14px}td,th{border:0;border-bottom:1px solid #e6dfd8;padding:10px 14px;text-align:left}th{font-weight:500;color:#141413}li::marker{color:#8e8b82}` + darkCode + `</style>`
}

// darkCode recolors highlight.js tokens for the design system's dark code surfaces.
const darkCode = `pre .hljs-comment,pre .hljs-quote{color:#8e8b82;font-style:italic}pre .hljs-keyword,pre .hljs-selector-tag,pre .hljs-built_in,pre .hljs-type,pre .hljs-meta{color:#e8a55a}pre .hljs-string,pre .hljs-regexp,pre .hljs-addition,pre .hljs-attribute,pre .hljs-attr{color:#9fc0d8}pre .hljs-number,pre .hljs-literal,pre .hljs-symbol,pre .hljs-variable,pre .hljs-template-variable{color:#e0a088}pre .hljs-title,pre .hljs-section,pre .hljs-name,pre .hljs-tag,pre .hljs-selector-class,pre .hljs-selector-id{color:#f0c9a8}pre .hljs-deletion{color:#e08080}pre .hljs-params,pre .hljs-subst,pre .hljs-punctuation,pre .hljs-property{color:#faf9f5}`
