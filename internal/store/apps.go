package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Apps are full-stack micro frontends: a frontend served from shartifacts and, optionally,
// a backend process it proxies to. Every deployment is a numbered, immutable version of the
// app's source tree with the build made from it; one version is live, any other can be
// previewed or promoted back (rollback). Data the backend keeps lives beside the versions
// and is shared by all of them.
//
// On disk, under apps/{id}/:
//
//	app.json        metadata, deployments, env, share links
//	v/{n}/src/      the version's source tree as deployed
//	v/{n}/out/      what the build made from it (rewritten pages, bundles, the server binary)
//	data/           the backend's persistent data (DATA_DIR)

// Limits of one deployment's source tree.
const (
	MaxAppFiles     = 5000
	MaxAppFileBytes = 64 << 20
	MaxAppBytes     = 256 << 20
	maxAppPath      = 300
	// KeepDeployments is how many recent versions keep their files; older ones are pruned
	// unless live or pinned by a share link.
	KeepDeployments = 20
)

// Deployment statuses.
const (
	StatusBuilding = "building"
	StatusReady    = "ready"
	StatusFailed   = "failed"
)

var (
	ErrAppNotFound = notFoundError("app not found")
	appSegment     = `[A-Za-z0-9_@+~][A-Za-z0-9._@+~-]*`
	appFilePath    = regexp.MustCompile(`^` + appSegment + `(/` + appSegment + `)*$`)
	envKey         = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)
	appLabel       = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)
	// generatedLabel is the shape of the hostname labels the server makes for apps without
	// one of their own; chosen labels never take it, so the two never collide.
	generatedLabel = regexp.MustCompile(`^[a-z2-7]{16}$`)
	// reservedEnv are set by shartifacts for every backend.
	reservedEnv = []string{"PORT", "HOST", "DATA_DIR", "APP_ID", "APP_VERSION", "APP_BASE_PATH", "PATH", "HOME", "TMPDIR"}
)

// notFoundError is a not-found error with its own message that still matches ErrNotFound.
type notFoundError string

func (e notFoundError) Error() string        { return string(e) }
func (e notFoundError) Is(target error) bool { return target == ErrNotFound }

type App struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Icon        string `json:"icon,omitempty"`
	Pinned      bool   `json:"pinned,omitempty"`
	// Label is the hostname label of the app's public links on the app origin, when it has
	// one of its own rather than the server's generated one. FormerLabels are the ones it
	// had before: they stay the app's, so links already handed out still reach it.
	Label        string    `json:"label,omitempty"`
	FormerLabels []string  `json:"former_labels,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	// Live is the version served at the app's live URL; 0 until one builds.
	Live int `json:"live"`
	// Env holds variables and secrets given to the backend; not versioned, never listed
	// with their values.
	Env         map[string]string `json:"env,omitempty"`
	Shares      []AppShare        `json:"shares,omitempty"`
	Deployments []Deployment      `json:"deployments"`
}

type Deployment struct {
	Version    int        `json:"version"`
	Note       string     `json:"note,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	Status     string     `json:"status"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Files      int        `json:"files"`
	Bytes      int64      `json:"bytes"`
	// Frontend is whether the version serves pages; Backend is how its server was made:
	// "go" (built from source here), "binary" (deployed prebuilt) or "" (none).
	Frontend bool   `json:"frontend"`
	Backend  string `json:"backend,omitempty"`
	// Network is whether the backend may open outbound connections.
	Network  bool     `json:"network,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	Error    string   `json:"error,omitempty"`
	// Log is the build's output, truncated.
	Log string `json:"log,omitempty"`
	// Pruned versions no longer keep their files.
	Pruned bool `json:"pruned,omitempty"`
}

// An AppShare is a public link to an app on the share origin. Version 0 follows whichever
// version is live; ReadOnly links pass only GET and HEAD requests to the backend.
type AppShare struct {
	Token     string    `json:"token"`
	Version   int       `json:"version"`
	ReadOnly  bool      `json:"read_only,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type AppSummary struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Icon        string    `json:"icon,omitempty"`
	Pinned      bool      `json:"pinned,omitempty"`
	Live        int       `json:"live"`
	Latest      int       `json:"latest"`
	Status      string    `json:"status"`
	Backend     string    `json:"backend,omitempty"`
	Frontend    bool      `json:"frontend"`
	Shares      int       `json:"shares,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// AppMeta creates or changes an app's descriptive fields; nil keeps a field.
type AppMeta struct {
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
	Icon        *string `json:"icon,omitempty"`
	Pinned      *bool   `json:"pinned,omitempty"`
	// Label "" returns the app to its generated hostname label.
	Label *string `json:"label,omitempty"`
}

// StageInput describes a new version's source tree.
type StageInput struct {
	// BaseVersion must be the latest version (0 for none), so a deploy never builds on a
	// tree it has not seen; negative skips the check.
	BaseVersion int
	// Merge starts from the latest version's files; otherwise the tree is exactly Files.
	Merge bool
	// Files are added or replaced; Remove deletes paths (and everything under a directory
	// path); Edits change text files in place, as artifact edits do.
	Files  map[string][]byte
	Remove []string
	Edits  []Edit
	Note   string
}

type appState struct {
	// deploy serializes staging of one app's versions.
	deploy sync.Mutex
}

func (s *Store) appsDir() string             { return filepath.Join(s.dir, "apps") }
func (s *Store) AppDir(id string) string     { return filepath.Join(s.appsDir(), id) }
func (s *Store) AppDataDir(id string) string { return filepath.Join(s.AppDir(id), "data") }
func (s *Store) versionDir(id string, v int) string {
	return filepath.Join(s.AppDir(id), "v", strconv.Itoa(v))
}
func (s *Store) AppSourceDir(id string, v int) string {
	return filepath.Join(s.versionDir(id, v), "src")
}
func (s *Store) AppBuildDir(id string, v int) string {
	return filepath.Join(s.versionDir(id, v), "out")
}

// AppCacheDir is where builds keep caches shared by every app (Go's build and module caches).
func (s *Store) AppCacheDir() string { return filepath.Join(s.appsDir(), ".cache") }

func (s *Store) loadApps() error {
	s.apps, s.appShares, s.appLabels, s.appStates = map[string]App{}, map[string]string{}, map[string]string{}, map[string]*appState{}
	files, err := filepath.Glob(filepath.Join(s.appsDir(), "*", "app.json"))
	if err != nil {
		return err
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		var a App
		if err = json.Unmarshal(b, &a); err != nil {
			return fmt.Errorf("read %s: %w", f, err)
		}
		if a.ID == "" || filepath.Base(filepath.Dir(f)) != a.ID {
			return fmt.Errorf("invalid app file %s", f)
		}
		changed := false
		for i, d := range a.Deployments {
			// A build the server did not live to finish.
			if d.Status == StatusBuilding {
				a.Deployments[i].Status, a.Deployments[i].Error = StatusFailed, "interrupted: shartifacts stopped during the build; deploy again"
				changed = true
			}
		}
		s.apps[a.ID] = a
		for _, sh := range a.Shares {
			s.appShares[sh.Token] = a.ID
		}
		for _, l := range a.labels() {
			s.appLabels[l] = a.ID
		}
		if changed {
			if _, err := s.writeApp(a); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeApp persists a and returns a copy the caller may change. s.mu is held.
func (s *Store) writeApp(a App) (App, error) {
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return App{}, err
	}
	if err = os.MkdirAll(s.AppDir(a.ID), 0700); err != nil {
		return App{}, err
	}
	if err = writeAtomic(s.AppDir(a.ID), filepath.Join(s.AppDir(a.ID), "app.json"), b); err != nil {
		return App{}, err
	}
	s.apps[a.ID] = a
	return copyApp(a), nil
}

func copyApp(a App) App {
	a.Deployments = append([]Deployment(nil), a.Deployments...)
	a.Shares = append([]AppShare(nil), a.Shares...)
	a.FormerLabels = append([]string(nil), a.FormerLabels...)
	env := make(map[string]string, len(a.Env))
	for k, v := range a.Env {
		env[k] = v
	}
	a.Env = env
	return a
}

// labels are the app's own hostname labels, current and former.
func (a App) labels() []string {
	if a.Label == "" {
		return a.FormerLabels
	}
	return append([]string{a.Label}, a.FormerLabels...)
}

func (a App) summary() AppSummary {
	out := AppSummary{ID: a.ID, Name: a.Name, Description: a.Description, Icon: a.Icon, Pinned: a.Pinned, Live: a.Live,
		Latest: len(a.Deployments), Shares: len(a.Shares), CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt}
	if n := len(a.Deployments); n > 0 {
		out.Status = a.Deployments[n-1].Status
	}
	if a.Live > 0 {
		d := a.Deployments[a.Live-1]
		out.Backend, out.Frontend = d.Backend, d.Frontend
	}
	return out
}

func validateMeta(m AppMeta, creating bool) error {
	if creating && m.Name == nil {
		return errors.New("name must contain 1–100 bytes")
	}
	if m.Name != nil && (strings.TrimSpace(*m.Name) == "" || len(*m.Name) > 100) {
		return errors.New("name must contain 1–100 bytes")
	}
	if m.Description != nil && len(*m.Description) > 300 {
		return errors.New("description exceeds 300 bytes")
	}
	if m.Icon != nil && (len(*m.Icon) > 32 || strings.ContainsAny(*m.Icon, "<>\"'&")) {
		return errors.New("icon must be one short word or an emoji")
	}
	if m.Label != nil && *m.Label != "" {
		switch l := *m.Label; {
		case !appLabel.MatchString(l) || strings.Contains(l, "--"):
			return errors.New("label must be 1–40 lowercase letters, digits and single hyphens, starting and ending with a letter or digit, like budget or team-notes")
		case generatedLabel.MatchString(l):
			return errors.New("16-character labels of only a–z and 2–7 are reserved for generated hostnames; choose another")
		}
	}
	return nil
}

// setLabel gives a its own hostname label ("" for the generated one), keeping the one it
// replaces as a former label. s.mu is held.
func (s *Store) setLabel(a *App, label string) error {
	if label == a.Label {
		return nil
	}
	if owner, ok := s.appLabels[label]; label != "" && ok && owner != a.ID {
		return fmt.Errorf("label %s is taken by another app", label)
	}
	if a.Label != "" {
		if len(a.FormerLabels) >= 20 {
			return errors.New("an app keeps at most 20 former labels; reuse one of them")
		}
		a.FormerLabels = append(a.FormerLabels, a.Label)
	}
	a.FormerLabels = slices.DeleteFunc(a.FormerLabels, func(l string) bool { return l == label })
	a.Label = label
	return nil
}

// indexLabels records a's labels after a has been written. s.mu is held.
func (s *Store) indexLabels(a App) {
	for _, l := range a.labels() {
		s.appLabels[l] = a.ID
	}
}

// AppByLabel finds the app a hostname label (one an app chose, current or former) is for.
func (s *Store) AppByLabel(label string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.appLabels[label]
	return id, ok
}

func applyMeta(a *App, m AppMeta) {
	if m.Name != nil {
		a.Name = strings.TrimSpace(*m.Name)
	}
	if m.Description != nil {
		a.Description = strings.TrimSpace(*m.Description)
	}
	if m.Icon != nil {
		a.Icon = strings.ToLower(strings.TrimSpace(*m.Icon))
	}
	if m.Pinned != nil {
		a.Pinned = *m.Pinned
	}
}

// Apps lists apps, pinned first, then most recently changed.
func (s *Store) Apps() []AppSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AppSummary, 0, len(s.apps))
	for _, a := range s.apps {
		out = append(out, a.summary())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pinned != out[j].Pinned {
			return out[i].Pinned
		}
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (s *Store) App(id string) (App, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.apps[id]
	if !ok {
		return App{}, ErrAppNotFound
	}
	return copyApp(a), nil
}

// CreateApp makes an app with no versions yet.
func (s *Store) CreateApp(m AppMeta) (App, error) {
	if err := validateMeta(m, true); err != nil {
		return App{}, err
	}
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return App{}, err
	}
	now := time.Now().UTC()
	a := App{ID: hex.EncodeToString(b), CreatedAt: now, UpdatedAt: now, Deployments: []Deployment{}}
	applyMeta(&a, m)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.apps) >= 500 {
		return App{}, errors.New("at most 500 apps; delete some first")
	}
	if m.Label != nil {
		if err := s.setLabel(&a, *m.Label); err != nil {
			return App{}, err
		}
	}
	s.appStates[a.ID] = &appState{}
	a, err := s.writeApp(a)
	if err == nil {
		s.indexLabels(a)
	}
	return a, err
}

// UpdateApp changes an app's name, description, icon, pin or hostname label.
func (s *Store) UpdateApp(id string, m AppMeta) (App, error) {
	if err := validateMeta(m, false); err != nil {
		return App{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.apps[id]
	if !ok {
		return App{}, ErrAppNotFound
	}
	a = copyApp(a)
	applyMeta(&a, m)
	if m.Label != nil {
		if err := s.setLabel(&a, *m.Label); err != nil {
			return App{}, err
		}
	}
	a.UpdatedAt = time.Now().UTC()
	a, err := s.writeApp(a)
	if err == nil {
		s.indexLabels(a)
	}
	return a, err
}

// DeleteApp removes an app with every version, its data and its links.
func (s *Store) DeleteApp(id string) error {
	s.mu.Lock()
	a, ok := s.apps[id]
	if !ok {
		s.mu.Unlock()
		return ErrAppNotFound
	}
	delete(s.apps, id)
	delete(s.appStates, id)
	for _, sh := range a.Shares {
		delete(s.appShares, sh.Token)
	}
	for _, l := range a.labels() {
		delete(s.appLabels, l)
	}
	s.mu.Unlock()
	return os.RemoveAll(s.AppDir(id))
}

func (s *Store) appState(id string) (*appState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.apps[id]; !ok {
		return nil, ErrAppNotFound
	}
	st := s.appStates[id]
	if st == nil {
		st = &appState{}
		s.appStates[id] = st
	}
	return st, nil
}

// ValidAppPath reports whether p may name a file in an app's source tree.
func ValidAppPath(p string) error {
	if len(p) > maxAppPath || !appFilePath.MatchString(p) {
		return fmt.Errorf("file path %q must be relative, like index.html or server/main.go (letters, digits, . _ - @ + ~; no leading dots)", p)
	}
	return nil
}

// StageDeployment writes a new version's source tree and records it as building. The
// caller builds it and reports with FinishDeployment.
func (s *Store) StageDeployment(id string, in StageInput) (Deployment, error) {
	st, err := s.appState(id)
	if err != nil {
		return Deployment{}, err
	}
	st.deploy.Lock()
	defer st.deploy.Unlock()
	a, err := s.App(id)
	if err != nil {
		return Deployment{}, err
	}
	latest := len(a.Deployments)
	if in.BaseVersion >= 0 && in.BaseVersion != latest {
		return Deployment{}, fmt.Errorf("%w (latest version is %d)", ErrConflict, latest)
	}
	if len(in.Note) > 1000 {
		return Deployment{}, errors.New("note exceeds 1000 bytes")
	}
	if !in.Merge && (len(in.Remove) > 0 || len(in.Edits) > 0) {
		return Deployment{}, errors.New("remove and edits change the previous version's files: deploy with merge")
	}
	if in.Merge && latest > 0 && a.Deployments[latest-1].Pruned {
		return Deployment{}, fmt.Errorf("version %d's files were pruned; deploy a complete tree instead", latest)
	}
	for p, b := range in.Files {
		if err := ValidAppPath(p); err != nil {
			return Deployment{}, err
		}
		if len(b) > MaxAppFileBytes {
			return Deployment{}, fmt.Errorf("file %s exceeds %d MiB", p, MaxAppFileBytes>>20)
		}
	}
	version := latest + 1
	vdir := s.versionDir(id, version)
	stage, err := os.MkdirTemp(filepath.Join(s.AppDir(id)), ".stage-*")
	if err != nil {
		return Deployment{}, err
	}
	defer os.RemoveAll(stage)
	src := filepath.Join(stage, "src")
	if err = os.MkdirAll(src, 0700); err != nil {
		return Deployment{}, err
	}
	removed := func(p string) bool {
		for _, r := range in.Remove {
			r = strings.TrimSuffix(r, "/")
			if p == r || strings.HasPrefix(p, r+"/") {
				return true
			}
		}
		return false
	}
	if in.Merge && latest > 0 {
		prev := s.AppSourceDir(id, latest)
		for _, r := range in.Remove {
			if _, err := os.Stat(filepath.Join(prev, filepath.FromSlash(strings.TrimSuffix(r, "/")))); err != nil {
				return Deployment{}, fmt.Errorf("remove %s: no such file in version %d", r, latest)
			}
		}
		err = filepath.WalkDir(prev, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(prev, path)
			rel = filepath.ToSlash(rel)
			if _, replaced := in.Files[rel]; replaced || removed(rel) {
				return nil
			}
			return linkOrCopy(path, filepath.Join(src, filepath.FromSlash(rel)))
		})
		if err != nil {
			return Deployment{}, err
		}
	}
	for p, b := range in.Files {
		if err = writeTreeFile(src, p, b); err != nil {
			return Deployment{}, err
		}
	}
	if len(in.Edits) > 0 {
		texts := map[string]string{}
		for i, e := range in.Edits {
			if e.File == "" {
				return Deployment{}, fmt.Errorf("edit %d: name the file it changes", i+1)
			}
			if _, ok := texts[e.File]; ok {
				continue
			}
			b, err := os.ReadFile(filepath.Join(src, filepath.FromSlash(e.File)))
			if err != nil || ValidAppPath(e.File) != nil {
				return Deployment{}, fmt.Errorf("edit %d: file %q does not exist", i+1, e.File)
			}
			texts[e.File] = string(b)
		}
		if _, err = ApplyEdits("", texts, in.Edits); err != nil {
			return Deployment{}, err
		}
		for p, body := range texts {
			// Unlink first: the file may be a hard link into the previous version.
			os.Remove(filepath.Join(src, filepath.FromSlash(p)))
			if err = writeTreeFile(src, p, []byte(body)); err != nil {
				return Deployment{}, err
			}
		}
	}
	files, total, err := treeSize(src)
	if err != nil {
		return Deployment{}, err
	}
	switch {
	case files == 0:
		return Deployment{}, errors.New("a deployment needs at least one file: index.html for a frontend, server/main.go for a backend")
	case files > MaxAppFiles:
		return Deployment{}, fmt.Errorf("a deployment holds at most %d files", MaxAppFiles)
	case total > MaxAppBytes:
		return Deployment{}, fmt.Errorf("a deployment holds at most %d MiB", MaxAppBytes>>20)
	}
	if err = os.MkdirAll(filepath.Dir(vdir), 0700); err != nil {
		return Deployment{}, err
	}
	os.RemoveAll(vdir) // leftovers of a version whose record never saved
	if err = os.Rename(stage, vdir); err != nil {
		return Deployment{}, err
	}
	d := Deployment{Version: version, Note: strings.TrimSpace(in.Note), CreatedAt: time.Now().UTC(), Status: StatusBuilding, Files: files, Bytes: total}
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.apps[id]
	if !ok {
		os.RemoveAll(vdir)
		return Deployment{}, ErrAppNotFound
	}
	a = copyApp(a)
	a.Deployments = append(a.Deployments, d)
	a.UpdatedAt = d.CreatedAt
	if _, err = s.writeApp(a); err != nil {
		os.RemoveAll(vdir)
		return Deployment{}, err
	}
	return d, nil
}

// FinishDeployment records a build's outcome and, when it is ready and promote is set,
// makes it live.
func (s *Store) FinishDeployment(id string, result Deployment, promote bool) (App, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.apps[id]
	if !ok {
		return App{}, ErrAppNotFound
	}
	if result.Version < 1 || result.Version > len(a.Deployments) {
		return App{}, errors.New("no such version")
	}
	a = copyApp(a)
	now := time.Now().UTC()
	d := &a.Deployments[result.Version-1]
	d.Status, d.FinishedAt = result.Status, &now
	d.Frontend, d.Backend, d.Network = result.Frontend, result.Backend, result.Network
	d.Warnings, d.Error = result.Warnings, result.Error
	if len(result.Log) > 64<<10 {
		result.Log = "…" + result.Log[len(result.Log)-64<<10:]
	}
	d.Log = result.Log
	// A slower, older build finishing late never replaces a newer live version.
	if promote && d.Status == StatusReady && d.Version > a.Live {
		a.Live = d.Version
	}
	a.UpdatedAt = now
	pruned := s.prune(&a)
	a, err := s.writeApp(a)
	if err != nil {
		return App{}, err
	}
	for _, v := range pruned {
		os.RemoveAll(s.versionDir(id, v))
	}
	return a, nil
}

// prune marks old versions' files for removal, keeping the newest KeepDeployments, the live
// version and versions share links pin. It returns the versions to remove.
func (s *Store) prune(a *App) []int {
	keep := map[int]bool{a.Live: true}
	for _, sh := range a.Shares {
		keep[sh.Version] = true
	}
	out := []int{}
	for i := range a.Deployments {
		d := &a.Deployments[i]
		if d.Pruned || keep[d.Version] || d.Version > len(a.Deployments)-KeepDeployments || d.Status == StatusBuilding {
			continue
		}
		d.Pruned = true
		out = append(out, d.Version)
	}
	return out
}

// SetLive promotes a ready version, or rolls back to one.
func (s *Store) SetLive(id string, version int) (App, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.apps[id]
	if !ok {
		return App{}, ErrAppNotFound
	}
	if version < 1 || version > len(a.Deployments) {
		return App{}, fmt.Errorf("version %d does not exist; latest is %d", version, len(a.Deployments))
	}
	if d := a.Deployments[version-1]; d.Status != StatusReady || d.Pruned {
		return App{}, fmt.Errorf("version %d cannot go live: it is %s", version, map[bool]string{true: "pruned", false: d.Status}[d.Pruned])
	}
	a = copyApp(a)
	a.Live = version
	a.UpdatedAt = time.Now().UTC()
	return s.writeApp(a)
}

// SetAppEnv adds, replaces (string) or removes (nil) backend environment variables.
func (s *Store) SetAppEnv(id string, changes map[string]*string) (App, error) {
	for k, v := range changes {
		if !envKey.MatchString(k) {
			return App{}, fmt.Errorf("variable %q must be UPPER_SNAKE_CASE, at most 64 bytes", k)
		}
		if slices.Contains(reservedEnv, k) || strings.HasPrefix(k, "SHARTIFACTS_") {
			return App{}, fmt.Errorf("%s is set by shartifacts", k)
		}
		if v != nil && (len(*v) > 8<<10 || strings.ContainsRune(*v, 0)) {
			return App{}, fmt.Errorf("%s: values are text of at most 8 KiB", k)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.apps[id]
	if !ok {
		return App{}, ErrAppNotFound
	}
	a = copyApp(a)
	for k, v := range changes {
		if v == nil {
			delete(a.Env, k)
		} else {
			a.Env[k] = *v
		}
	}
	if len(a.Env) > 100 {
		return App{}, errors.New("an app holds at most 100 variables")
	}
	if len(a.Env) == 0 {
		a.Env = nil
	}
	return s.writeApp(a)
}

// CreateAppShare makes a public link to the app (version 0: whichever version is live).
func (s *Store) CreateAppShare(id string, version int, readOnly bool) (AppShare, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.apps[id]
	if !ok {
		return AppShare{}, ErrAppNotFound
	}
	if version < 0 || version > len(a.Deployments) {
		return AppShare{}, errors.New("no such version")
	}
	if version > 0 && (a.Deployments[version-1].Status != StatusReady || a.Deployments[version-1].Pruned) {
		return AppShare{}, fmt.Errorf("version %d cannot be shared: share a ready version that still has its files", version)
	}
	if len(a.Shares) >= 100 {
		return AppShare{}, errors.New("at most 100 links per app; revoke some first")
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return AppShare{}, err
	}
	sh := AppShare{Token: hex.EncodeToString(b), Version: version, ReadOnly: readOnly, CreatedAt: time.Now().UTC()}
	a = copyApp(a)
	a.Shares = append(a.Shares, sh)
	if _, err := s.writeApp(a); err != nil {
		return AppShare{}, err
	}
	s.appShares[sh.Token] = id
	return sh, nil
}

// AppShared resolves a share token to its link and app.
func (s *Store) AppShared(token string) (AppShare, App, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.apps[s.appShares[token]]
	if !ok {
		return AppShare{}, App{}, false
	}
	for _, sh := range a.Shares {
		if sh.Token == token {
			return sh, copyApp(a), true
		}
	}
	return AppShare{}, App{}, false
}

// RevokeAppShare ends a link; false when there was none.
func (s *Store) RevokeAppShare(token string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.apps[s.appShares[token]]
	if !ok {
		return false, nil
	}
	a = copyApp(a)
	a.Shares = slices.DeleteFunc(a.Shares, func(sh AppShare) bool { return sh.Token == token })
	if _, err := s.writeApp(a); err != nil {
		return false, err
	}
	delete(s.appShares, token)
	return true, nil
}

// AppFiles lists a version's source files with their sizes.
func (s *Store) AppFiles(id string, version int) (map[string]int64, error) {
	a, err := s.App(id)
	if err != nil {
		return nil, err
	}
	if version < 1 || version > len(a.Deployments) {
		return nil, fmt.Errorf("version %d does not exist", version)
	}
	if a.Deployments[version-1].Pruned {
		return nil, fmt.Errorf("version %d's files were pruned", version)
	}
	root := s.AppSourceDir(id, version)
	out := map[string]int64{}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		out[filepath.ToSlash(rel)] = info.Size()
		return nil
	})
	return out, err
}

// ReadAppFile reads one file of a version's source tree.
func (s *Store) ReadAppFile(id string, version int, p string) ([]byte, error) {
	if err := ValidAppPath(p); err != nil {
		return nil, err
	}
	a, err := s.App(id)
	if err != nil {
		return nil, err
	}
	if version < 1 || version > len(a.Deployments) || a.Deployments[version-1].Pruned {
		return nil, fmt.Errorf("version %d has no files", version)
	}
	b, err := os.ReadFile(filepath.Join(s.AppSourceDir(id, version), filepath.FromSlash(p)))
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("version %d has no file %s", version, p)
	}
	return b, err
}

func writeTreeFile(root, p string, b []byte) error {
	dst := filepath.Join(root, filepath.FromSlash(p))
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return fmt.Errorf("file %s: a file of that name is in the way of its directory", p)
	}
	if err := os.WriteFile(dst, b, 0600); err != nil {
		return fmt.Errorf("file %s: %w", p, err)
	}
	return nil
}

// linkOrCopy shares an unchanged file with the previous version; versions are immutable.
func linkOrCopy(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return err
	}
	if os.Link(src, dst) == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func treeSize(root string) (files int, total int64, err error) {
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files++
		total += info.Size()
		return nil
	})
	return
}

// AppsDir is the directory holding every app; a runner sidecar mounts it.
func (s *Store) AppsDir() string { return s.appsDir() }

// Dir is the data directory.
func (s *Store) Dir() string { return s.dir }

// Secret returns a random 32-byte key kept in meta/{name}.key, creating it on first use.
func (s *Store) Secret(name string) ([]byte, error) {
	path := filepath.Join(s.dir, "meta", name+".key")
	if b, err := os.ReadFile(path); err == nil && len(b) == 32 {
		return b, nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if err := writeAtomic(filepath.Dir(path), path, b); err != nil {
		return nil, err
	}
	return b, nil
}
