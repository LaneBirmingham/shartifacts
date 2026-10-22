package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDocDB(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	a, _ := s.Save("", Input{Title: "Board", Kind: "html", Content: "x"})
	sam, ada := Viewer{ID: "u_sam", Owner: true}, Viewer{ID: "u_ada"}
	for _, bad := range []string{"tasks", "a//b", "a/../b", "a/b c", strings.Repeat("a/", 17) + "a"} {
		if e := s.DocWrite(a.ID, "set", bad, map[string]any{}, sam); !errors.Is(e, ErrInvalidArgument) {
			t.Fatalf("%q accepted: %v", bad, e)
		}
	}
	for i, title := range []string{"b", "a", "c"} {
		if e := s.DocWrite(a.ID, "set", "tasks/t"+string(rune('1'+i)), map[string]any{"title": title, "n": float64(i), "tags": []any{"x"}}, sam); e != nil {
			t.Fatal(e)
		}
	}
	if e := s.DocWrite(a.ID, "update", "tasks/t1", map[string]any{"meta": map[string]any{"done": true}}, ada); e != nil {
		t.Fatal(e)
	}
	if e := s.DocWrite(a.ID, "update", "tasks/missing", map[string]any{"x": 1.0}, ada); !errors.Is(e, ErrInvalidArgument) {
		t.Fatal("update created a document")
	}
	d, _ := s.DocGet(a.ID, "tasks/t1", ada)
	if !d.Exists || d.Version != 2 || d.Data["meta"].(map[string]any)["done"] != true || d.Data["title"] != "b" {
		t.Fatal(d)
	}
	docs, _ := s.DocQuery(a.ID, "tasks", DocQuery{OrderBy: "title"}, ada)
	if len(docs) != 3 || docs[0].Data["title"] != "a" {
		t.Fatal(docs)
	}
	docs, _ = s.DocQuery(a.ID, "tasks", DocQuery{Where: []DocFilter{{"n", ">=", 1.0}, {"tags", "array-contains", "x"}}, OrderBy: "n", Dir: "desc", Limit: 1}, ada)
	if len(docs) != 1 || docs[0].ID != "t3" {
		t.Fatal(docs)
	}
	// Each person's data/users/<id>/ subtree is private, from the owner too.
	if e := s.DocWrite(a.ID, "set", "data/users/u_ada/prefs", map[string]any{"theme": "dark"}, ada); e != nil {
		t.Fatal(e)
	}
	if d, _ = s.DocGet(a.ID, "data/users/u_ada/prefs", sam); d.Exists {
		t.Fatal("owner read another person's private document")
	}
	if e := s.DocWrite(a.ID, "set", "data/users/u_ada/prefs", map[string]any{}, sam); !errors.Is(e, ErrInvalidArgument) {
		t.Fatal("owner wrote another person's private document")
	}
	if docs, _ = s.DocQuery(a.ID, "data/users/u_ada", DocQuery{}, sam); len(docs) != 0 {
		t.Fatal("query leaked a private document")
	}
	// Declared rules: everyone reads votes, only the owner changes another's vote.
	caps := json.RawMessage(`{"db":{"rules":[{"path":"votes","read":"view","write":"owner"},{"path":"votes/{self}","write":"interact"}]}}`)
	s.Save(a.ID, Input{Capabilities: caps, Edits: []Edit{}, BaseVersion: 1})
	if e := s.DocWrite(a.ID, "set", "votes/u_ada", map[string]any{"pick": 1.0}, ada); e != nil {
		t.Fatal(e)
	}
	if e := s.DocWrite(a.ID, "set", "votes/u_sam", map[string]any{"pick": 2.0}, ada); !errors.Is(e, ErrInvalidArgument) {
		t.Fatal("wrote someone else's vote")
	}
	if d, _ = s.DocGet(a.ID, "votes/u_ada", sam); !d.Exists {
		t.Fatal("a rule at the prefix should open siblings for reading")
	}
	// Leases.
	r, _ := s.DocAcquire(a.ID, "locks/edit", "tab1", time.Second*5, nil, sam)
	busy, _ := s.DocAcquire(a.ID, "locks/edit", "tab2", 0, nil, ada)
	if !r.Acquired || busy.Acquired || busy.ExpiresAt == "" {
		t.Fatal(r, busy)
	}
	reopened, _ := Open(dir)
	if d, _ = reopened.DocGet(a.ID, "tasks/t3", ada); !d.Exists || d.Data["title"] != "c" {
		t.Fatal("db not durable")
	}
	reopened.Delete(a.ID, 2)
	if _, e := reopened.DocGet(a.ID, "tasks/t3", ada); !errors.Is(e, ErrNotFound) {
		t.Fatal("db outlived its artifact")
	}
}

func TestAssetsAndPeople(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	a, _ := s.Save("", Input{Title: "Wall", Kind: "html", Content: "x"})
	png := "\x89PNG\r\n\x1a\nrest"
	for _, tc := range []struct{ typ, body string }{{"image/png; x=1", png}, {"text/html", "<p>"}, {"image/png", "<svg>"}, {"text/css", "<style>"}, {"text/plain", "\xff\xfe"}, {"image/png", ""}} {
		if _, e := s.UploadAsset(a.ID, tc.typ, "", strings.NewReader(tc.body)); e == nil {
			t.Fatalf("accepted %q %q", tc.typ, tc.body)
		}
	}
	asset, e := s.UploadAsset(a.ID, "image/png", "u_sam", strings.NewReader(png))
	if e != nil || len(asset.ID) != 32 || asset.URL != "/_blob/"+asset.ID {
		t.Fatal(e, asset)
	}
	if path, typ, e := s.AssetFile(asset.ID); e != nil || typ != "image/png" || !strings.HasSuffix(path, asset.ID) {
		t.Fatal(e)
	}
	reopened, _ := Open(dir)
	list, usage, _ := reopened.Assets(a.ID)
	if len(list) != 1 || usage.Bytes != int64(len(png)) {
		t.Fatal(list, usage)
	}
	if deleted, _ := reopened.DeleteAsset(a.ID, asset.ID); !deleted {
		t.Fatal("not deleted")
	}
	if _, _, e = reopened.AssetFile(asset.ID); !errors.Is(e, ErrNotFound) {
		t.Fatal("deleted asset still served")
	}
	sam, _ := s.AddPerson("  Sam  ")
	again, _ := s.AddPerson("sam")
	if sam.ID != again.ID || !strings.HasPrefix(sam.ID, "u_") || sam.Color == "" {
		t.Fatal(sam, again)
	}
	if _, e = s.AddPerson(""); e == nil {
		t.Fatal("empty name accepted")
	}
	if p, ok := reopened.Person(sam.ID); ok || len(reopened.People()) != 0 {
		t.Fatal("reopened before the person was added should not see them", p)
	}
	if third, _ := Open(dir); len(third.People()) != 1 {
		t.Fatal("people not durable")
	}
}
