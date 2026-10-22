package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestRevisionsPersistenceAndConflicts(t *testing.T) {
	dir := t.TempDir()
	s, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	in := Input{Title: "Timer", Kind: "html", Content: "<h1>One</h1>"}
	a, e := s.Save("", in)
	if e != nil {
		t.Fatal(e)
	}
	in.Content = "<h1>Two</h1>"
	in.BaseVersion = 1
	b, e := s.Save(a.ID, in)
	if e != nil {
		t.Fatal(e)
	}
	if len(b.Revisions) != 2 || b.Revisions[0].Content != "<h1>One</h1>" {
		t.Fatalf("history changed: %+v", b)
	}
	if _, e = s.Save(a.ID, in); !errors.Is(e, ErrConflict) {
		t.Fatalf("stale update accepted: %v", e)
	}
	b.Revisions[0].Content = "bad"
	again, _ := s.Get(a.ID)
	if again.Revisions[0].Content != "<h1>One</h1>" {
		t.Fatal("caller can mutate storage")
	}
	reloaded, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	got, _ := reloaded.Get(a.ID)
	if len(got.Revisions) != 2 || got.Revisions[1].Content != "<h1>Two</h1>" {
		t.Fatal("history not durable")
	}
	if e = s.Delete(a.ID, 1); !errors.Is(e, ErrConflict) {
		t.Fatalf("stale delete accepted: %v", e)
	}
	if e = s.Delete(a.ID, 2); e != nil {
		t.Fatal(e)
	}
	reloaded, e = Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	if len(reloaded.List()) != 0 {
		t.Fatal("deletion not durable")
	}
}
func TestValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   Input
	}{
		{"blank title", Input{Title: "  ", Kind: "html"}}, {"unknown kind", Input{Title: "a", Kind: "exe"}}, {"too large", Input{Title: "a", Kind: "html", Content: string(make([]byte, MaxContent+1))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := Open(t.TempDir())
			if _, e := s.Save("", tc.in); e == nil {
				t.Fatal("invalid input accepted")
			}
			if len(s.List()) != 0 {
				t.Fatal("invalid input persisted")
			}
		})
	}
}
func TestConcurrentWriters(t *testing.T) {
	s, _ := Open(t.TempDir())
	in := Input{Title: "a", Kind: "code", Content: "1"}
	a, _ := s.Save("", in)
	in.BaseVersion = 1
	var wg sync.WaitGroup
	results := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := s.Save(a.ID, in); results <- e }()
	}
	wg.Wait()
	close(results)
	success := 0
	for e := range results {
		if e == nil {
			success++
		} else if !errors.Is(e, ErrConflict) {
			t.Fatal(e)
		}
	}
	if success != 1 {
		t.Fatalf("got %d concurrent successes", success)
	}
}
func TestCorruptStorageFailsWithoutOverwriting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	original := []byte("{broken")
	os.WriteFile(path, original, 0600)
	if _, e := Open(dir); e == nil {
		t.Fatal("corrupt data ignored")
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(original) {
		t.Fatal("corrupt data overwritten")
	}
}

func TestEditsDeriveFromLatest(t *testing.T) {
	s, e := Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	a, e := s.Save("", Input{Title: "Page", Kind: "html", Content: "<h1>One</h1><p>a</p><p>a</p>"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Save("", Input{Title: "x", Kind: "html", Edits: []Edit{}}); e == nil {
		t.Fatal("edits accepted on create")
	}
	for _, tc := range []struct {
		edits []Edit
		want  string
	}{
		{[]Edit{{OldStr: "<p>a</p>", NewStr: "<p>b</p>"}}, "occurs 2 times"},
		{[]Edit{{OldStr: "<h2>", NewStr: "x"}}, "edit 1: old_str not found"},
		{[]Edit{{OldStr: "One", NewStr: "Two"}, {OldStr: "", NewStr: "x"}}, "edit 2: old_str is empty"},
	} {
		if _, e = s.Save(a.ID, Input{Edits: tc.edits, BaseVersion: 1}); e == nil || !strings.Contains(e.Error(), tc.want) {
			t.Fatalf("want %q, got %v", tc.want, e)
		}
	}
	if got, _ := s.Get(a.ID); len(got.Revisions) != 1 {
		t.Fatal("a failed edit saved a revision")
	}
	if _, e = s.Save(a.ID, Input{Content: "x", Edits: []Edit{{OldStr: "One", NewStr: "Two"}}, BaseVersion: 1}); e == nil {
		t.Fatal("content and edits accepted together")
	}
	// Later edits see earlier ones; omitted title and kind are kept.
	b, e := s.Save(a.ID, Input{Edits: []Edit{{OldStr: "One", NewStr: "Two"}, {OldStr: "<h1>Two", NewStr: "<h1 id=t>Two"}}, BaseVersion: 1})
	if e != nil {
		t.Fatal(e)
	}
	r := b.Revisions[1]
	if r.Content != "<h1 id=t>Two</h1><p>a</p><p>a</p>" || r.Title != "Page" || r.Kind != "html" {
		t.Fatalf("%+v", r)
	}
	if _, e = s.Save(a.ID, Input{Edits: []Edit{{OldStr: "Two", NewStr: "Three"}}, BaseVersion: 1}); !errors.Is(e, ErrConflict) {
		t.Fatalf("stale edit accepted: %v", e)
	}
	// An empty edit list keeps the source and changes only metadata.
	c, e := s.Save(a.ID, Input{Title: "Renamed", Edits: []Edit{}, BaseVersion: 2})
	if e != nil || c.Revisions[2].Content != r.Content || c.Revisions[2].Title != "Renamed" {
		t.Fatalf("%v %+v", e, c.Revisions[2])
	}
}

func TestSupportingFiles(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	css, js := "h1{color:red}", "export const n = 1;"
	for _, bad := range []map[string]*string{{"../x.css": &css}, {"/abs.css": &css}, {".hidden": &css}, {"a//b.js": &css}, {"ok.css": nil}} {
		if _, e := s.Save("", Input{Title: "x", Kind: "html", Content: "x", Files: bad}); e == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
	a, e := s.Save("", Input{Title: "Site", Kind: "html", Content: `<link rel="stylesheet" href="style.css">`, Files: map[string]*string{"style.css": &css, "lib/app.js": &js}})
	if e != nil || len(a.Revisions[0].Files) != 2 {
		t.Fatal(e, a)
	}
	// Merge: replace one file, remove another, keep the rest; edits may target a file.
	page := "about"
	b, e := s.Save(a.ID, Input{Files: map[string]*string{"lib/app.js": nil, "about.html": &page}, Edits: []Edit{{OldStr: "red", NewStr: "blue", File: "style.css"}}, BaseVersion: 1})
	if e != nil {
		t.Fatal(e)
	}
	r := b.Revisions[1]
	if r.Content != a.Revisions[0].Content || len(r.Files) != 2 || r.Files["style.css"] != "h1{color:blue}" || r.Files["about.html"] != "about" {
		t.Fatalf("%+v", r)
	}
	if _, e = s.Save(a.ID, Input{Edits: []Edit{{OldStr: "x", NewStr: "y", File: "missing.css"}}, BaseVersion: 2}); e == nil || !strings.Contains(e.Error(), "does not exist") {
		t.Fatal(e)
	}
	// Removing every file leaves none recorded; older revisions keep theirs.
	c, _ := s.Save(a.ID, Input{Files: map[string]*string{"style.css": nil, "about.html": nil}, Edits: []Edit{}, BaseVersion: 2})
	if c.Revisions[2].Files != nil || len(c.Revisions[0].Files) != 2 {
		t.Fatal(c.Revisions)
	}
	reopened, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	got, _ := reopened.Get(a.ID)
	if got.Revisions[1].Files["style.css"] != "h1{color:blue}" {
		t.Fatal("files not durable")
	}
	big := strings.Repeat("x", MaxContent)
	files := map[string]*string{}
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		files[n+".txt"] = &big
	}
	if _, e = s.Save("", Input{Title: "big", Kind: "html", Content: "x", Files: files}); e == nil {
		t.Fatal("over 8 MiB accepted")
	}
}
