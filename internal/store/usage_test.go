package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func failed(t *testing.T, s *Store, id string, v int) App {
	t.Helper()
	a, err := s.FinishDeployment(id, Deployment{Version: v, Status: StatusFailed, Error: "go build failed"}, false)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func stage(t *testing.T, s *Store, id string, base int, files map[string]string) {
	t.Helper()
	in := StageInput{BaseVersion: base, Files: map[string][]byte{}}
	for p, b := range files {
		in.Files[p] = []byte(b)
	}
	if _, err := s.StageDeployment(id, in); err != nil {
		t.Fatal(err)
	}
}

func TestFailedBuildsDoNotShrinkTheRollbackWindow(t *testing.T) {
	s, _ := Open(t.TempDir())
	s.KeepVersions = 3
	a := newApp(t, s)
	for v := 1; v <= 3; v++ {
		stage(t, s, a.ID, v-1, map[string]string{"index.html": "x"})
		ready(t, s, a.ID, v, v == 3)
	}
	// Five failed builds in a row would have pruned every good version under a window of
	// deploys; counted in ready versions, all three stay.
	for v := 4; v <= 8; v++ {
		stage(t, s, a.ID, v-1, map[string]string{"index.html": "x"})
		a = failed(t, s, a.ID, v)
	}
	pruned := func(a App) string {
		var out []string
		for _, d := range a.Deployments {
			if d.Pruned {
				out = append(out, d.Status[:1]+string(rune('0'+d.Version)))
			}
		}
		return strings.Join(out, ",")
	}
	// The failed ones keep their source only while among the newest three deploys.
	if got := pruned(a); got != "f4,f5" {
		t.Fatalf("pruned %s", got)
	}
	if a.OldestRollback() != 1 {
		t.Fatalf("oldest rollback %d", a.OldestRollback())
	}
	stage(t, s, a.ID, 8, map[string]string{"index.html": "x"})
	a = ready(t, s, a.ID, 9, false)
	if got := pruned(a); got != "r1,f4,f5,f6" {
		t.Fatalf("pruned %s", got)
	}
	if a.OldestRollback() != 2 {
		t.Fatalf("oldest rollback %d", a.OldestRollback())
	}
	if _, err := os.Stat(s.AppSourceDir(a.ID, 1)); !os.IsNotExist(err) {
		t.Fatalf("pruned version's files remain: %v", err)
	}
}

func TestDeploysShareUnchangedFiles(t *testing.T) {
	s, _ := Open(t.TempDir())
	a := newApp(t, s)
	stage(t, s, a.ID, 0, map[string]string{"index.html": "<h1>same</h1>", "app.js": "one", "lib/big.js": strings.Repeat("x", 1000)})
	ready(t, s, a.ID, 1, true)
	// A full deploy, not a merge, resending everything with one file changed.
	stage(t, s, a.ID, 1, map[string]string{"index.html": "<h1>same</h1>", "app.js": "two", "lib/big.js": strings.Repeat("x", 1000)})
	ready(t, s, a.ID, 2, true)
	same := func(p string) bool {
		i1, err1 := os.Stat(filepath.Join(s.AppSourceDir(a.ID, 1), p))
		i2, err2 := os.Stat(filepath.Join(s.AppSourceDir(a.ID, 2), p))
		if err1 != nil || err2 != nil {
			t.Fatal(err1, err2)
		}
		return os.SameFile(i1, i2)
	}
	if !same("index.html") || !same("lib/big.js") || same("app.js") {
		t.Fatalf("index.html %v, lib/big.js %v, app.js %v", same("index.html"), same("lib/big.js"), same("app.js"))
	}
	for v, want := range map[int]string{1: "one", 2: "two"} {
		if b, _ := s.ReadAppFile(a.ID, v, "app.js"); string(b) != want {
			t.Fatalf("v%d app.js %q", v, b)
		}
	}
	// A same-sized file with other bytes is written, not linked.
	stage(t, s, a.ID, 2, map[string]string{"index.html": "<h1>diff</h1>", "app.js": "two", "lib/big.js": strings.Repeat("x", 1000)})
	if b, _ := s.ReadAppFile(a.ID, 3, "index.html"); string(b) != "<h1>diff</h1>" {
		t.Fatalf("v3 index.html %q", b)
	}
	if b, _ := s.ReadAppFile(a.ID, 2, "index.html"); string(b) != "<h1>same</h1>" {
		t.Fatalf("v2 index.html changed: %q", b)
	}

	if runtime.GOOS == "windows" {
		return // no inodes: shared files count once per name
	}
	if err := os.MkdirAll(s.AppDataDir(a.ID), 0700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(s.AppDataDir(a.ID), "notes.json"), []byte("0123456789"), 0600)
	u, err := s.AppUsage(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	// index.html: two copies; app.js: two; lib/big.js: one shared by all three versions.
	want := AppUsage{Versions: 3, VersionsFiles: 5, VersionsBytes: 13*2 + 3*2 + 1000, DataBytes: 10, DataFiles: 1}
	if u != want {
		t.Fatalf("usage %+v, want %+v", u, want)
	}
	if _, err := s.AppUsage("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestArtifactFilesAreCompact(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	a, err := s.Save("", Input{Title: "Tags", Kind: "html", Content: "<p>a & b</p>"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, a.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	// HTML escaping would grow every < > & sixfold, and indentation every line.
	if bytes.Contains(b, []byte(`\u0026`)) || bytes.Contains(b, []byte("\n ")) || !bytes.Contains(b, []byte("<p>a & b</p>")) {
		t.Fatalf("%s", b)
	}
	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := again.Get(a.ID); got.Revisions[0].Content != "<p>a & b</p>" {
		t.Fatalf("%+v", got)
	}
}

func TestSavesToDifferentArtifactsRunTogether(t *testing.T) {
	s, _ := Open(t.TempDir())
	var ids []string
	for i := 0; i < 4; i++ {
		a, err := s.Save("", Input{Title: "a", Kind: "code", Content: "0"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, a.ID)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for v := 1; v <= 10; v++ {
				_, err := s.Save(id, Input{Title: "a", Kind: "code", Content: strings.Repeat("x", v), BaseVersion: v})
				errs <- err
				s.List(ExcludeArchived)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range ids {
		if a, _ := s.Get(id); len(a.Revisions) != 11 || a.Revisions[10].Content != strings.Repeat("x", 10) {
			t.Fatalf("%s: %d revisions", id, len(a.Revisions))
		}
	}
	// Deleting while a save waits leaves neither a resurrected artifact nor a stuck lock.
	if err := s.Delete(ids[0], 11); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(ids[0], Input{Title: "a", Kind: "code", Content: "y", BaseVersion: 11}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestMediaUsageAndDeletion(t *testing.T) {
	s, _ := Open(t.TempDir())
	upload := func(mime, body, name string) Media {
		t.Helper()
		m, err := s.SaveMedia(mime, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if name != "" {
			if m, err = s.NameMedia(m, name); err != nil {
				t.Fatal(err)
			}
		}
		return m
	}
	logo := upload("image/png", "png bytes", "logo.png")
	sales := upload("text/csv", "a,b\n1,2\n", "sales.csv")
	clip := upload("video/mp4", "mp4 bytes", "")
	saved := upload("image/png", "saved in data", "")
	unused := upload("image/png", "nobody", "unused.png")

	// By URL in the latest revision; by quoted name only in an earlier one, after an
	// apostrophe that would pair with the name's opening quote.
	if _, err := s.Save("", Input{Title: "Page", Kind: "html", Content: `<img src="` + logo.URL + `">`}); err != nil {
		t.Fatal(err)
	}
	b, _ := s.Save("", Input{Title: "Chart", Kind: "react", Content: `// don't block: await window.fs.readFile('sales.csv')`})
	if _, err := s.Save(b.ID, Input{Title: "Chart", Kind: "react", Content: "export default () => null", BaseVersion: 1}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(s.storagePath(b.ID)), 0700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(s.storagePath(b.ID), []byte(`{"photo":"/media/`+saved.ID+`"}`), 0600)
	app := newApp(t, s)
	stage(t, s, app.ID, 0, map[string]string{"index.html": "<video src='media/" + clip.ID + "'></video>", "photo.bin": unused.ID})

	all, err := s.ListMedia()
	if err != nil {
		t.Fatal(err)
	}
	usage, err := s.MediaUsage(all)
	if err != nil {
		t.Fatal(err)
	}
	user := func(m Media) string {
		var out []string
		for _, u := range usage[m.ID] {
			out = append(out, u.Kind+":"+u.Title)
		}
		return strings.Join(out, ",")
	}
	for m, want := range map[Media]string{logo: "artifact:Page", sales: "artifact:Chart", saved: "artifact:Chart", clip: "app:Tool", unused: ""} {
		if got := user(m); got != want {
			t.Errorf("%s used by %q, want %q", m.Name+m.ID[60:], got, want)
		}
	}

	if err := s.DeleteMedia(unused.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteMedia(unused.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	for _, bad := range []string{"../meta/shares.json", "nope.png", strings.Repeat("a", 64) + ".exe"} {
		if err := s.DeleteMedia(bad); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: %v", bad, err)
		}
	}
	all, _ = s.ListMedia()
	if len(all) != 4 {
		t.Fatalf("%d left", len(all))
	}
	// Its name goes with it, so an upload of the same bytes starts unnamed.
	if again := upload("image/png", "nobody", ""); again.ID != unused.ID {
		t.Fatal(again)
	}
	all, _ = s.ListMedia()
	for _, m := range all {
		if m.ID == unused.ID && m.Name != "" {
			t.Fatalf("name survived deletion: %q", m.Name)
		}
	}
}
