package server

import (
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

var (
	externalAttr = regexp.MustCompile(`(?i)<(script|link|img|video|audio|source|track|iframe|embed|object|image|use)\b[^>]*?\s(?:src|href|xlink:href|data)\s*=\s*["']?((?:https?:)?//[^"'\s>]+)`)
	externalCSS  = regexp.MustCompile(`(?i)(?:@import\s+(?:url\()?|url\()\s*["']?((?:https?:)?//[^"')\s]+)`)
	externalJS   = regexp.MustCompile("(?i)\\b(?:fetch|XMLHttpRequest\\.open|new\\s+WebSocket|new\\s+EventSource)\\s*\\(\\s*(?:[\"'`]GET[\"'`]\\s*,\\s*)?[\"'`]((?:https?|wss?):)?//([^\"'`\\s]+)")
	markdownImg  = regexp.MustCompile(`!\[[^\]]*\]\(\s*<?(https?://[^)\s>]+)`)
	linkTag      = regexp.MustCompile(`(?i)<link\b[^>]*>`)
	loadedLink   = regexp.MustCompile(`(?i)rel\s*=\s*["']?(stylesheet|preload|modulepreload|icon)`)
)

// checkArtifact finds problems an agent cannot see without a browser: React that does not
// compile, and external resources the preview sandbox will block. Saving still succeeds.
func checkArtifact(kind, content string, files map[string]string) []string {
	var problems []string
	blocked := map[string]bool{}
	block := func(url string) {
		if !blocked[url] && len(blocked) < 5 {
			blocked[url] = true
			problems = append(problems, "blocked by the sandbox: "+url+" (only bundled libraries, /media/ files and data: URLs load; upload files to the Media library)")
		}
	}
	switch kind {
	case "react":
		if _, _, _, e := compileReact(content, files); e != nil {
			problems = append(problems, "React does not compile: "+e.Error())
		}
	case "html", "svg":
		for _, m := range externalAttr.FindAllStringSubmatch(content, -1) {
			if strings.EqualFold(m[1], "script") && cdnLibrary(m[2]) != "" {
				continue // served locally by localizeScripts
			}
			if strings.EqualFold(m[1], "link") {
				continue // checked below only when it is a stylesheet or font
			}
			block(m[2])
		}
		for _, m := range linkTag.FindAllString(content, -1) {
			if href := externalAttr.FindStringSubmatch(m); href != nil && loadedLink.MatchString(m) {
				block(href[2])
			}
		}
	case "markdown", "slides":
		for _, m := range markdownImg.FindAllStringSubmatch(content, -1) {
			block(m[1])
		}
	}
	if kind == "html" || kind == "svg" || kind == "react" || kind == "markdown" || kind == "slides" {
		for _, m := range externalCSS.FindAllStringSubmatch(content, -1) {
			block(m[1])
		}
	}
	for _, p := range slices.Sorted(maps.Keys(files)) {
		sub := map[string]string{".html": "html", ".htm": "html", ".css": "css", ".js": "js", ".mjs": "js", ".svg": "svg"}[path.Ext(p)]
		for _, problem := range checkArtifact(sub, files[p], nil) {
			problems = append(problems, p+": "+problem)
		}
	}
	if kind == "css" {
		for _, m := range externalCSS.FindAllStringSubmatch(content, -1) {
			block(m[1])
		}
	}
	if kind == "html" || kind == "react" || kind == "js" {
		for _, m := range externalJS.FindAllStringSubmatch(content, -1) {
			problems = append(problems, fmt.Sprintf("network requests are blocked: %s//%s (keep data in the source, window.storage or window.fs.readFile)", strings.TrimSuffix(m[1], ":")+":", m[2]))
			break
		}
	}
	return problems
}

// runtimeErrors keeps the latest uncaught errors browsers saw in each saved version, in
// memory only, so an agent can learn what broke without a browser of its own.
type runtimeErrors struct {
	mu    sync.Mutex
	items map[string][]RuntimeError
}

type RuntimeError struct {
	Version int       `json:"version"`
	Message string    `json:"message"`
	Count   int       `json:"count"`
	LastAt  time.Time `json:"last_at"`
}

func (r *runtimeErrors) add(id string, version int, message string) {
	if len(message) > 1000 {
		message = message[:1000] + "…"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.items == nil {
		r.items = map[string][]RuntimeError{}
	}
	list := r.items[id]
	for i, e := range list {
		if e.Version == version && e.Message == message {
			list[i].Count++
			list[i].LastAt = time.Now().UTC()
			return
		}
	}
	list = append(list, RuntimeError{version, message, 1, time.Now().UTC()})
	if len(list) > 20 {
		list = list[len(list)-20:]
	}
	r.items[id] = list
}

func (r *runtimeErrors) forVersion(id string, version int) []RuntimeError {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []RuntimeError
	for _, e := range r.items[id] {
		if e.Version == version {
			out = append(out, e)
		}
	}
	return out
}

// reportError records an error a preview reported through the page that embeds it.
func (s *Server) reportError(id string, in struct {
	Version int    `json:"version"`
	Message string `json:"message"`
}) error {
	a, e := s.Store.Get(id)
	if e != nil {
		return e
	}
	if in.Version < 1 || in.Version > len(a.Revisions) || strings.TrimSpace(in.Message) == "" {
		return fmt.Errorf("an error report needs a saved version and a message")
	}
	s.errors.add(id, in.Version, strings.TrimSpace(in.Message))
	return nil
}
