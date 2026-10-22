package search

import (
	"html"
	"path"
	"regexp"
	"strings"
	"unicode"
)

var (
	htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	rawBlock    = regexp.MustCompile(`(?is)<(script|style)\b[^>]*>(.*?)</(?:script|style)\s*>`)
	headingTag  = regexp.MustCompile(`(?is)<(?:h[1-6]|title)\b[^>]*>(.*?)</(?:h[1-6]|title)\s*>`)
	textAttr    = regexp.MustCompile(`(?i)\s(?:alt|title|placeholder|aria-label)\s*=\s*("[^"]*"|'[^']*')`)
	anyTag      = regexp.MustCompile(`(?s)<[^>]*>`)
	mdHeading   = regexp.MustCompile(`(?m)^ {0,3}#{1,6}[ \t]+(.+)$`)
	mdFence     = regexp.MustCompile("(?ms)^ {0,3}(?:```|~~~)[^\n]*\n(.*?)^ {0,3}(?:```|~~~)[ \t]*$")
	mdLinkURL   = regexp.MustCompile(`\]\([^)]*\)`)
	mdMarks     = regexp.MustCompile("\\*{1,3}|_{2,}|`+|(?m)^ {0,3}#{1,6}[ \t]|(?m)^ {0,3}>|\\||(?m)^ {0,3}-{3,}[ \t]*$")
	importLine  = regexp.MustCompile(`(?m)^\s*(?:import\b|export\s+\*\s+from\b).*$`)
	literal     = regexp.MustCompile("\"(?:[^\"\\\\\n]|\\\\.)*\"|'(?:[^'\\\\\n]|\\\\.)*'|`(?:[^`\\\\]|\\\\.)*`")
	jsxText     = regexp.MustCompile(`>([^<>{}]*[A-Za-z][^<>{}]*)<`)
)

// text is an artifact's searchable text by role.
type text struct {
	headings, body, code []string
}

// add extracts the readable text of one source by its type.
func (t *text) add(kind, src string) {
	switch kind {
	case "html", "svg":
		t.html(src)
	case "markdown", "slides":
		t.markdown(src)
	case "react":
		t.code = append(t.code, src)
		t.body = append(t.body, prose(src, true)...)
	case "script":
		t.code = append(t.code, src)
		t.body = append(t.body, prose(src, false)...)
	case "style":
		t.code = append(t.code, src)
	default: // code, mermaid and data files are read as they are
		t.body = append(t.body, src)
	}
}

func (t *text) html(src string) {
	src = htmlComment.ReplaceAllString(src, " ")
	for _, m := range rawBlock.FindAllStringSubmatch(src, -1) {
		t.code = append(t.code, m[2])
		if strings.EqualFold(m[1], "script") {
			t.body = append(t.body, prose(m[2], false)...)
		}
	}
	src = rawBlock.ReplaceAllString(src, " ")
	for _, m := range headingTag.FindAllStringSubmatch(src, -1) {
		t.headings = append(t.headings, plain(m[1]))
	}
	t.body = append(t.body, plain(src))
	for _, m := range textAttr.FindAllStringSubmatch(src, -1) {
		t.body = append(t.body, html.UnescapeString(m[1][1:len(m[1])-1]))
	}
}

func (t *text) markdown(src string) {
	for _, m := range mdFence.FindAllStringSubmatch(src, -1) {
		t.code = append(t.code, m[1])
	}
	src = mdFence.ReplaceAllString(src, " ")
	for _, m := range mdHeading.FindAllStringSubmatch(src, -1) {
		t.headings = append(t.headings, mdMarks.ReplaceAllString(plain(m[1]), ""))
	}
	src = mdLinkURL.ReplaceAllString(src, "]")
	t.body = append(t.body, mdMarks.ReplaceAllString(plain(src), " "))
}

func plain(s string) string { return html.UnescapeString(anyTag.ReplaceAllString(s, " ")) }

// prose finds the words a script shows people: string literals that read as text and,
// for JSX, text between tags. Imports, class names and identifiers stay code.
func prose(code string, jsx bool) []string {
	code = importLine.ReplaceAllString(code, "")
	var out []string
	for _, m := range literal.FindAllString(code, -1) {
		if s := m[1 : len(m)-1]; readable(s) {
			out = append(out, s)
		}
	}
	if jsx {
		for _, m := range jsxText.FindAllStringSubmatch(code, -1) {
			out = append(out, m[1])
		}
	}
	return out
}

// readable reports whether a string literal is text for people, not a class list, path
// or keyword: mostly plain words, and a lone word only when capitalised.
func readable(s string) bool {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return false
	}
	if len(fields) == 1 {
		r := []rune(fields[0])
		return len(r) >= 3 && unicode.IsUpper(r[0]) && plainWord(fields[0])
	}
	n := 0
	for _, f := range fields {
		if plainWord(strings.Trim(f, `.,!?:;()"'`)) {
			n++
		}
	}
	return n*10 >= len(fields)*6
}

func plainWord(w string) bool {
	if w == "" {
		return false
	}
	for _, r := range w {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\'' && r != '&' && r != '$' {
			return false
		}
	}
	return true
}

// fileRole maps a supporting file to how its text is read.
func fileRole(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".md", ".markdown":
		return "markdown"
	case ".html", ".htm", ".svg":
		return "html"
	case ".jsx", ".tsx":
		return "react"
	case ".js", ".mjs", ".ts":
		return "script"
	case ".css":
		return "style"
	}
	return "data"
}

var (
	mediaRef = regexp.MustCompile(`(?i)/media/[0-9a-z]+\.(mp4|webm|mov|m4v|mp3|wav|m4a|ogg|oga|flac|png|jpe?g|gif|webp|avif|svg)\b`)
	traitRes = map[string]*regexp.Regexp{
		"video":   regexp.MustCompile(`(?i)<video\b`),
		"audio":   regexp.MustCompile(`(?i)<audio\b|from\s+["']tone["']|\bTone\.`),
		"image":   regexp.MustCompile(`(?i)<img\b|!\[[^\]]*\]\(`),
		"chart":   regexp.MustCompile(`(?i)from\s+["'](?:recharts|chart\.js|plotly[^"']*|d3)["']|\bnew Chart\(|\bPlotly\.|chart\.js|plotly|<canvas[^>]*chart`),
		"table":   regexp.MustCompile(`(?i)<table\b|(?m)^\s*\|?\s*:?-{3,}:?\s*\|`),
		"form":    regexp.MustCompile(`(?i)<form\b|<input\b|<textarea\b|<select\b`),
		"3d":      regexp.MustCompile(`(?i)from\s+["']three["']|three(?:\.min)?\.js|\bTHREE\.`),
		"math":    regexp.MustCompile(`\$\$|\\\(|\\\[|katex|mathjs`),
		"diagram": regexp.MustCompile("(?i)```mermaid"),
		"data":    regexp.MustCompile(`(?i)window\.fs\.readFile|papaparse|xlsx|claude\.use\(\s*["']db["']|window\.storage`),
	}
	// traitHints are words a source must contain before its trait's pattern is worth running.
	traitHints = map[string][]string{
		"video": {"<video"}, "audio": {"<audio", "tone"}, "image": {"<img", "!["},
		"chart": {"recharts", "chart", "plotly", "d3"}, "table": {"<table", "---"},
		"form": {"<form", "<input", "<textarea", "<select"}, "3d": {"three"},
		"math": {"$$", "\\(", "\\[", "katex", "mathjs"}, "diagram": {"```mermaid"},
		"data": {"readfile", "papaparse", "xlsx", "claude.use", "window.storage"},
	}
	mediaTraits = map[string]string{"mp4": "video", "webm": "video", "mov": "video", "m4v": "video", "mp3": "audio", "wav": "audio", "m4a": "audio", "ogg": "audio", "oga": "audio", "flac": "audio"}
	// kindWords describe each type in the words people use for it.
	kindWords = map[string]string{
		"html": "html page web", "react": "react app component", "markdown": "markdown document",
		"slides": "slides presentation", "svg": "svg image vector drawing", "mermaid": "mermaid diagram",
		"code": "code snippet",
	}
)

// traits are what an artifact contains (video, chart, form…), for has: filters and for
// finding things by what they are rather than what they say.
func traits(kind string, sources []string) []string {
	found := map[string]bool{}
	switch kind {
	case "svg":
		found["image"] = true
	case "mermaid":
		found["diagram"] = true
	}
	for _, src := range sources {
		lower := strings.ToLower(src)
		if strings.Contains(lower, "/media/") {
			for _, m := range mediaRef.FindAllStringSubmatch(src, -1) {
				if t, ok := mediaTraits[strings.ToLower(m[1])]; ok {
					found[t] = true
				} else {
					found["image"] = true
				}
			}
		}
		for name, re := range traitRes {
			if found[name] {
				continue
			}
			for _, hint := range traitHints[name] {
				if strings.Contains(lower, hint) {
					found[name] = re.MatchString(src)
					break
				}
			}
		}
	}
	var out []string
	for _, name := range traitOrder {
		if found[name] {
			out = append(out, name)
		}
	}
	return out
}

var traitOrder = []string{"video", "audio", "image", "chart", "diagram", "table", "form", "3d", "math", "data"}

// Traits lists the has: values traits produce, for help text.
func Traits() []string { return append([]string(nil), traitOrder...) }
