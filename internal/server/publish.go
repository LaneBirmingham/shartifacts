package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"regexp"
	"slices"
	"strconv"

	"github.com/LaneBirmingham/shartifacts/internal/store"
)

// publish is the `artifact` capability's write: a page replaces itself with a whole new
// HTML document, or changes some of its files. It is compare-and-set against the version
// the page is running, or, when every file names the copy it replaces (ifMatch, a sha256),
// against just those files, so two people saving different files both land.
// POST /api/artifacts/{id}/publish

var doctype = regexp.MustCompile(`(?is)^\s*<!doctype\s+html`)

type publishFile struct {
	Content *string `json:"content,omitempty"`
	Delete  bool    `json:"delete,omitempty"`
	IfMatch *string `json:"if_match"`
	Checked bool    `json:"checked,omitempty"`
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func (s *Server) publish(w http.ResponseWriter, r *http.Request) {
	var in struct {
		BaseVersion int                     `json:"base_version"`
		HTML        *string                 `json:"html,omitempty"`
		Files       map[string]*publishFile `json:"files,omitempty"`
	}
	if !decode(w, r, &in) {
		return
	}
	a, e := s.Store.Get(r.PathValue("id"))
	if e != nil {
		fail(w, e)
		return
	}
	latest := a.Revisions[len(a.Revisions)-1]
	failWith := func(status int, body map[string]any) { writeJSON(w, status, body) }
	if (in.HTML == nil) == (in.Files == nil) {
		failWith(400, map[string]any{"code": "invalid_content", "error": "publish a whole page (html) or files, not both"})
		return
	}
	next := store.Input{BaseVersion: len(a.Revisions), Note: "Published by the page", Owner: ""}
	result := map[string]any{}
	if in.HTML != nil {
		if !doctype.MatchString(*in.HTML) {
			failWith(400, map[string]any{"code": "invalid_content", "error": "a page must begin with <!doctype html>"})
			return
		}
		if in.BaseVersion != len(a.Revisions) {
			failWith(409, map[string]any{"code": "conflict", "error": "a newer version was published", "live": strconv.Itoa(len(a.Revisions))})
			return
		}
		next.Kind, next.Content, next.Edits = "html", *in.HTML, nil
	} else {
		// The page itself is index.html; every other path is a supporting file.
		perFile := len(in.Files) > 0
		for _, f := range in.Files {
			perFile = perFile && f != nil && f.Checked
		}
		current := func(p string) (string, bool) {
			if p == "index.html" && latest.Kind == "html" {
				return latest.Content, true
			}
			body, ok := latest.Files[p]
			return body, ok
		}
		if perFile {
			conflicts := []map[string]any{}
			for _, p := range slices.Sorted(mapsKeys(in.Files)) {
				body, ok := current(p)
				var actual any
				if ok {
					actual = sha(body)
				}
				expected := in.Files[p].IfMatch
				if (expected == nil) != !ok || (expected != nil && ok && *expected != sha(body)) {
					conflicts = append(conflicts, map[string]any{"path": p, "expected": expected, "actual": actual})
				}
			}
			if len(conflicts) > 0 {
				failWith(409, map[string]any{"code": "conflict", "error": "files changed since this page read them", "paths": conflicts, "live": strconv.Itoa(len(a.Revisions))})
				return
			}
			// Report what other writers changed since the version this page runs.
			if in.BaseVersion >= 1 && in.BaseVersion < len(a.Revisions) {
				base := a.Revisions[in.BaseVersion-1]
				changed := []map[string]any{}
				for _, p := range slices.Sorted(mapsKeys(unionFiles(base.Files, latest.Files))) {
					if _, named := in.Files[p]; named || base.Files[p] == latest.Files[p] {
						continue
					}
					var now any
					if body, ok := latest.Files[p]; ok {
						now = sha(body)
					}
					changed = append(changed, map[string]any{"path": p, "sha256": now})
				}
				result["changed"] = changed
			} else {
				result["changed"] = []any{}
			}
		} else if in.BaseVersion != len(a.Revisions) {
			failWith(409, map[string]any{"code": "conflict", "error": "a newer version was published", "live": strconv.Itoa(len(a.Revisions))})
			return
		}
		files := map[string]*string{}
		next.Edits = []store.Edit{}
		for p, f := range in.Files {
			switch {
			case f == nil || f.Delete:
				if p == "index.html" && latest.Kind == "html" {
					failWith(400, map[string]any{"code": "invalid_content", "error": "the page itself cannot be deleted"})
					return
				}
				files[p] = nil
			case f.Content == nil:
				failWith(400, map[string]any{"code": "invalid_content", "error": "file " + p + " needs content"})
				return
			case p == "index.html" && latest.Kind == "html":
				next.Content, next.Edits = *f.Content, nil
			default:
				files[p] = f.Content
			}
		}
		next.Files = files
	}
	saved, e := s.Store.Save(a.ID, next)
	if e != nil {
		status, code := codeFor(e)
		if code == "invalid_argument" {
			code = "invalid_content"
		}
		if code == "conflict" {
			status = 409
		}
		failWith(status, map[string]any{"code": code, "error": e.Error()})
		return
	}
	rev := saved.Revisions[len(saved.Revisions)-1]
	result["version"] = strconv.Itoa(rev.Version)
	if in.Files != nil {
		shas := map[string]string{}
		for p, f := range in.Files {
			if f != nil && f.Checked && !f.Delete {
				if body, ok := rev.Files[p]; ok {
					shas[p] = sha(body)
				} else if p == "index.html" {
					shas[p] = sha(rev.Content)
				}
			}
		}
		if len(shas) > 0 || hasChecked(in.Files) {
			result["shas"] = shas
		}
	}
	writeJSON(w, 200, result)
}

func hasChecked(files map[string]*publishFile) bool {
	for _, f := range files {
		if f != nil && f.Checked {
			return true
		}
	}
	return false
}

func mapsKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func unionFiles(a, b map[string]string) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}
