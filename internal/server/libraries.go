package server

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// libraries maps each importable module path to its bundle in web/vendor/lib, built by
// scripts/build.mjs. Keep the two in sync.
var libraries = map[string]string{
	"lucide-react": "lucide-react",
	"recharts":     "recharts",
	"d3":           "d3",
	"mathjs":       "mathjs",
	"three":        "three",
	"xlsx":         "xlsx",
	"tone":         "tone",
	"chart.js":     "chart", "chart.js/auto": "chart",
	"lodash":    "lodash",
	"papaparse": "papaparse",
	"plotly":    "plotly", "plotly.js": "plotly", "plotly.js-dist": "plotly", "plotly.js-dist-min": "plotly",
	"highlight.js": "highlight",
	"katex":        "katex",
}

// hasMath spots TeX delimiters, so Markdown loads KaTeX only when it needs it.
var hasMath = regexp.MustCompile(`\$\$|\\\(|\\\[|\$\S[^$\n]*\S\$|\$\S\$`)

// libraryFor resolves an import path to its bundle, including shadcn/ui-style paths.
func libraryFor(path string) (lib, member string, ok bool) {
	if lib, ok = libraries[path]; ok {
		return lib, "", true
	}
	if strings.HasPrefix(path, "@/components/ui/") || path == "@/lib/utils" {
		return "ui", "", true
	}
	if fn, found := strings.CutPrefix(path, "lodash/"); found && validMember.MatchString(fn) {
		return "lodash", fmt.Sprintf("[%q]", fn), true
	}
	return "", "", false
}

var validMember = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

// shadcnTheme gives Tailwind the shadcn/ui color tokens (bg-primary, text-muted-foreground,
// border-input...) that its components and artifacts written for them use.
const shadcnTheme = `<style type="text/tailwindcss">@theme{--color-background:hsl(0 0% 100%);--color-foreground:hsl(222.2 84% 4.9%);--color-card:hsl(0 0% 100%);--color-card-foreground:hsl(222.2 84% 4.9%);--color-popover:hsl(0 0% 100%);--color-popover-foreground:hsl(222.2 84% 4.9%);--color-primary:hsl(222.2 47.4% 11.2%);--color-primary-foreground:hsl(210 40% 98%);--color-secondary:hsl(210 40% 96.1%);--color-secondary-foreground:hsl(222.2 47.4% 11.2%);--color-muted:hsl(210 40% 96.1%);--color-muted-foreground:hsl(215.4 16.3% 46.9%);--color-accent:hsl(210 40% 96.1%);--color-accent-foreground:hsl(222.2 47.4% 11.2%);--color-destructive:hsl(0 84.2% 60.2%);--color-destructive-foreground:hsl(210 40% 98%);--color-border:hsl(214.3 31.8% 91.4%);--color-input:hsl(214.3 31.8% 91.4%);--color-ring:hsl(222.2 84% 4.9%);--radius:0.5rem}@layer base{*,::before,::after{border-color:var(--color-border)}}</style>`

// reactModules are the globals set by web/vendor/react.js, shared by every library bundle.
var reactModules = map[string]string{
	"react":                 "React",
	"react-dom":             "ReactDOM",
	"react-dom/client":      "ReactDOMClient",
	"react/jsx-runtime":     "ReactJSXRuntime",
	"react/jsx-dev-runtime": "ReactJSXRuntime",
}

// LibraryNames lists the import paths agents can use, for guidance and error messages.
func LibraryNames() string {
	names := []string{"react", "react-dom", "react-dom/client"}
	for path := range libraries {
		names = append(names, path)
	}
	names = append(names, "lodash/*", "@/components/ui/* (shadcn/ui)", "@/lib/utils")
	sort.Strings(names[3:])
	return strings.Join(names, ", ")
}

// Tailwind's preflight resets unstyled elements, so it is only added to React artifacts
// that visibly use utility classes rather than to every one.
var (
	classAttr    = regexp.MustCompile("className\\s*=\\s*(?:\"([^\"]*)\"|'([^']*)'|\\{\\s*`([^`]*)`)")
	tailwindUtil = regexp.MustCompile(`^(?:[a-z0-9-]+:)*-?(?:flex|grid|inline-flex|inline-block|hidden|block|relative|absolute|fixed|sticky|container|truncate|uppercase|italic|underline|items-[a-z]+|justify-[a-z]+|content-[a-z]+|self-[a-z]+|place-[a-z-]+|gap(?:-[xy])?-[\d.]+|space-[xy]-[\d.]+|[mp][xytrbl]?-(?:[\d.]+|auto|px)|(?:min-|max-)?[wh]-(?:full|screen|auto|min|max|fit|[a-z0-9./]+)|text-[a-z0-9-/]+|font-[a-z]+|leading-[a-z0-9]+|tracking-[a-z]+|bg-[a-z0-9-/]+|from-[a-z0-9-/]+|to-[a-z0-9-/]+|rounded(?:-[a-z0-9]+)*|shadow(?:-[a-z0-9]+)?|border(?:-[a-z0-9-/]+)?|ring(?:-[a-z0-9-/]+)?|opacity-\d+|z-\d+|inset-[a-z0-9-]+|top-[a-z0-9-/]+|left-[a-z0-9-/]+|right-[a-z0-9-/]+|bottom-[a-z0-9-/]+|overflow-[a-z-]+|transition(?:-[a-z]+)?|duration-\d+|cursor-[a-z-]+|grid-cols-\d+|col-span-\d+|flex-[a-z0-9]+)$`)
)

func usesTailwind(source string) bool {
	hits := 0
	for _, m := range classAttr.FindAllStringSubmatch(source, -1) {
		for _, token := range strings.Fields(m[1] + " " + m[2] + " " + m[3]) {
			if tailwindUtil.MatchString(token) {
				if hits++; hits >= 3 {
					return true
				}
			}
		}
	}
	return false
}

var (
	scriptTag   = regexp.MustCompile(`(?is)<script\b[^>]*>`)
	scriptSrc   = regexp.MustCompile(`(?is)\bsrc\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)
	integrity   = regexp.MustCompile(`(?is)\s+integrity\s*=\s*(?:"[^"]*"|'[^']*'|[^\s>]+)`)
	moduleType  = regexp.MustCompile(`(?is)\btype\s*=\s*["']?module\b`)
	cdnjs       = regexp.MustCompile(`^https?://cdnjs\.cloudflare\.com/ajax/libs/([^/]+)/`)
	npmCDN      = regexp.MustCompile(`^https?://(?:cdn\.jsdelivr\.net/npm|unpkg\.com)/((?:@[^/@]+/)?[^/@]+)`)
	cdnPackages = map[string]string{
		"chart.js": "chart", "d3": "d3", "three": "three", "three.js": "three", "lodash": "lodash", "lodash.js": "lodash",
		"papaparse": "papaparse", "mathjs": "mathjs", "math.js": "mathjs", "plotly.js": "plotly", "plotly.js-dist": "plotly",
		"plotly.js-dist-min": "plotly", "tone": "tone", "xlsx": "xlsx", "@tailwindcss/browser": "tailwind",
		"highlight.js": "highlight", "@highlightjs/cdn-assets": "highlight",
	}
)

// cdnLibrary names the vendored bundle that stands in for a well-known CDN script URL.
func cdnLibrary(url string) string {
	switch {
	case strings.HasPrefix(url, "https://cdn.tailwindcss.com"), strings.HasPrefix(url, "http://cdn.tailwindcss.com"):
		return "tailwind"
	case strings.HasPrefix(url, "https://cdn.plot.ly/"):
		return "plotly"
	case strings.HasPrefix(url, "https://cdn.sheetjs.com/"):
		return "xlsx"
	}
	if m := cdnjs.FindStringSubmatch(url); m != nil {
		return cdnPackages[strings.ToLower(m[1])]
	}
	if m := npmCDN.FindStringSubmatch(url); m != nil {
		return cdnPackages[strings.ToLower(m[1])]
	}
	return ""
}

// localizeScripts serves classic CDN script tags for the bundled libraries from this
// server, so HTML written for a CDN works offline inside the preview's CSP. The saved
// source is not changed. Module scripts are left alone: their imports name the CDN.
func localizeScripts(html, origin string) string {
	return scriptTag.ReplaceAllStringFunc(html, func(tag string) string {
		if moduleType.MatchString(tag) {
			return tag
		}
		m := scriptSrc.FindStringSubmatchIndex(tag)
		if m == nil {
			return tag
		}
		url := ""
		for g := 2; g < 8; g += 2 {
			if m[g] >= 0 {
				url = tag[m[g]:m[g+1]]
			}
		}
		lib := cdnLibrary(strings.TrimSpace(url))
		if lib == "" {
			return tag
		}
		// A CDN's subresource-integrity hash cannot match the local file.
		tag = tag[:m[0]] + `src="` + origin + "/vendor/lib/" + lib + `.js"` + tag[m[1]:]
		return integrity.ReplaceAllString(tag, "")
	})
}
