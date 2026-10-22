package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newApp(t *testing.T, s *Store) App {
	t.Helper()
	name := "Tool"
	a, err := s.CreateApp(AppMeta{Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func ready(t *testing.T, s *Store, id string, v int, promote bool) App {
	t.Helper()
	a, err := s.FinishDeployment(id, Deployment{Version: v, Status: StatusReady, Frontend: true}, promote)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAppVersionsAreImmutableTrees(t *testing.T) {
	s, _ := Open(t.TempDir())
	a := newApp(t, s)
	d, err := s.StageDeployment(a.ID, StageInput{BaseVersion: 0, Files: map[string][]byte{"index.html": []byte("<h1>one</h1>"), "server/main.go": []byte("package main"), "img/logo.png": {0x89, 'P'}}})
	if err != nil || d.Version != 1 || d.Status != StatusBuilding || d.Files != 3 {
		t.Fatalf("stage: %+v %v", d, err)
	}
	ready(t, s, a.ID, 1, true)
	// Merge: change one file by edit, drop a directory, add a file; v1 stays as it was.
	d, err = s.StageDeployment(a.ID, StageInput{BaseVersion: 1, Merge: true,
		Edits:  []Edit{{File: "index.html", OldStr: "one", NewStr: "two"}},
		Remove: []string{"img"},
		Files:  map[string][]byte{"app.js": []byte("1")}})
	if err != nil || d.Version != 2 || d.Files != 3 {
		t.Fatalf("merge: %+v %v", d, err)
	}
	v1, _ := s.ReadAppFile(a.ID, 1, "index.html")
	v2, _ := s.ReadAppFile(a.ID, 2, "index.html")
	if string(v1) != "<h1>one</h1>" || string(v2) != "<h1>two</h1>" {
		t.Fatalf("an edit leaked into the previous version: %q %q", v1, v2)
	}
	if _, err := s.ReadAppFile(a.ID, 2, "img/logo.png"); err == nil {
		t.Fatal("removed directory survived")
	}
	files, _ := s.AppFiles(a.ID, 2)
	if _, ok := files["server/main.go"]; !ok || len(files) != 3 {
		t.Fatalf("v2 files: %v", files)
	}
	// A stale base, a bad path, an edit that misses and remove without merge are refused.
	if _, err := s.StageDeployment(a.ID, StageInput{BaseVersion: 1, Files: map[string][]byte{"a": nil}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale base: %v", err)
	}
	for _, p := range []string{"../x", ".env", "a//b", "/abs", "a/../b"} {
		if _, err := s.StageDeployment(a.ID, StageInput{BaseVersion: -1, Files: map[string][]byte{p: nil}}); err == nil {
			t.Fatalf("path %q accepted", p)
		}
	}
	if _, err := s.StageDeployment(a.ID, StageInput{BaseVersion: 2, Merge: true, Edits: []Edit{{File: "index.html", OldStr: "absent", NewStr: ""}}}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missed edit: %v", err)
	}
	if _, err := s.StageDeployment(a.ID, StageInput{BaseVersion: 2, Remove: []string{"app.js"}}); err == nil {
		t.Fatal("remove without merge accepted")
	}
	if a, _ = s.App(a.ID); len(a.Deployments) != 2 {
		t.Fatalf("refused deploys left versions: %d", len(a.Deployments))
	}
}

func TestAppLiveVersion(t *testing.T) {
	s, _ := Open(t.TempDir())
	a := newApp(t, s)
	for v := 1; v <= 3; v++ {
		if _, err := s.StageDeployment(a.ID, StageInput{BaseVersion: v - 1, Files: map[string][]byte{"index.html": []byte("x")}}); err != nil {
			t.Fatal(err)
		}
	}
	ready(t, s, a.ID, 3, true)
	// An older build finishing later does not replace the newer live version.
	if a = ready(t, s, a.ID, 1, true); a.Live != 3 {
		t.Fatalf("late build went live: %d", a.Live)
	}
	if _, err := s.FinishDeployment(a.ID, Deployment{Version: 2, Status: StatusFailed, Error: "boom"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetLive(a.ID, 2); err == nil {
		t.Fatal("failed version went live")
	}
	if a, _ = s.SetLive(a.ID, 1); a.Live != 1 {
		t.Fatalf("rollback: %d", a.Live)
	}
}

func TestAppEnvSharesAndReload(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	a := newApp(t, s)
	s.StageDeployment(a.ID, StageInput{BaseVersion: 0, Files: map[string][]byte{"index.html": []byte("x")}})
	ready(t, s, a.ID, 1, true)
	key := "secret"
	if _, err := s.SetAppEnv(a.ID, map[string]*string{"API_KEY": &key}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"PORT", "lower", "SHARTIFACTS_X", "DATA_DIR"} {
		if _, err := s.SetAppEnv(a.ID, map[string]*string{bad: &key}); err == nil {
			t.Fatalf("variable %s accepted", bad)
		}
	}
	sh, err := s.CreateAppShare(a.ID, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	// A build interrupted by a restart is failed on load, not left building forever.
	s.StageDeployment(a.ID, StageInput{BaseVersion: 1, Files: map[string][]byte{"index.html": []byte("y")}})

	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s2.App(a.ID)
	if err != nil || got.Env["API_KEY"] != "secret" || got.Live != 1 {
		t.Fatalf("reload: %+v %v", got, err)
	}
	if d := got.Deployments[1]; d.Status != StatusFailed || !strings.Contains(d.Error, "interrupted") {
		t.Fatalf("interrupted build: %+v", d)
	}
	if found, app, ok := s2.AppShared(sh.Token); !ok || app.ID != a.ID || !found.ReadOnly {
		t.Fatal("share lost on reload")
	}
	if revoked, _ := s2.RevokeAppShare(sh.Token); !revoked {
		t.Fatal("revoke")
	}
	if _, _, ok := s2.AppShared(sh.Token); ok {
		t.Fatal("revoked share still resolves")
	}
	if err := s2.DeleteApp(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "apps", a.ID)); !os.IsNotExist(err) {
		t.Fatal("app files survived deletion")
	}
	if _, err := s2.App(a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted app: %v", err)
	}
}

// Labels are unique among apps, current and former alike, and survive a reload.
func TestAppLabels(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	a, b := newApp(t, s), newApp(t, s)
	set := func(id, l string) error {
		_, err := s.UpdateApp(id, AppMeta{Label: &l})
		return err
	}
	if err := set(a.ID, "budget"); err != nil {
		t.Fatal(err)
	}
	if err := set(a.ID, "money"); err != nil {
		t.Fatal(err)
	}
	for _, l := range []string{"budget", "money"} {
		if err := set(b.ID, l); err == nil {
			t.Fatalf("%s taken twice", l)
		}
	}
	s2, _ := Open(dir)
	got, _ := s2.App(a.ID)
	if got.Label != "money" || len(got.FormerLabels) != 1 || got.FormerLabels[0] != "budget" {
		t.Fatalf("reload: %+v", got)
	}
	if id, ok := s2.AppByLabel("budget"); !ok || id != a.ID {
		t.Fatal("former label lost on reload")
	}
	s2.DeleteApp(a.ID)
	if _, ok := s2.AppByLabel("money"); ok {
		t.Fatal("deleted app's label still taken")
	}
}

func TestAppPruning(t *testing.T) {
	s, _ := Open(t.TempDir())
	a := newApp(t, s)
	total := KeepDeployments + 5
	for v := 1; v <= total; v++ {
		s.StageDeployment(a.ID, StageInput{BaseVersion: v - 1, Files: map[string][]byte{"index.html": []byte("x")}})
		ready(t, s, a.ID, v, v == 1) // version 1 stays live
		if v == 2 {
			// A link pinned to version 2 keeps its files.
			if _, err := s.CreateAppShare(a.ID, 2, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := s.CreateAppShare(a.ID, 3, false); err == nil {
		t.Fatal("shared a pruned version")
	}
	s.StageDeployment(a.ID, StageInput{BaseVersion: total, Files: map[string][]byte{"index.html": []byte("x")}})
	a = ready(t, s, a.ID, total+1, false)
	for _, d := range a.Deployments {
		keep := d.Version == 1 || d.Version == 2 || d.Version > total+1-KeepDeployments
		if d.Pruned == keep {
			t.Fatalf("version %d pruned=%v", d.Version, d.Pruned)
		}
		if _, err := os.Stat(s.AppSourceDir(a.ID, d.Version)); (err == nil) != keep {
			t.Fatalf("version %d files exist=%v", d.Version, err == nil)
		}
	}
}
