package store

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

var ErrNotFound = errors.New("artifact not found")
var ErrConflict = errors.New("artifact changed: reload the latest version before saving")

// ErrNotArchived refuses to delete an artifact in bulk while it is still in the library.
var ErrNotArchived = errors.New("artifact is not archived: archive it first, or delete it on its own with confirmation")

const MaxContent = 2 << 20

// A revision's supporting files: text served beside its page by relative path.
const (
	MaxFiles    = 100
	MaxRevision = 8 << 20
	maxFilePath = 200
	fileSegment = `[A-Za-z0-9_][A-Za-z0-9._-]*`
)

var filePath = regexp.MustCompile(`^` + fileSegment + `(/` + fileSegment + `)*$`)

var Kinds = []string{"html", "react", "markdown", "slides", "svg", "mermaid", "code"}

type Revision struct {
	Version int    `json:"version"`
	Title   string `json:"title"`
	Kind    string `json:"kind"`
	Content string `json:"content"`
	// Files are supporting files by relative path (style.css, lib/chart.js, data.json),
	// which the page references relatively. Most artifacts have none.
	Files     map[string]string `json:"files,omitempty"`
	Note      string            `json:"note"`
	CreatedAt time.Time         `json:"created_at"`
}
type Artifact struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	// Description is a one-sentence subtitle for library cards; Pinned keeps the
	// artifact at the top of the library. Neither is versioned.
	Description string `json:"description,omitempty"`
	// Icon is one short generic word (chart, recipe, map) or an emoji, as Claude's.
	Icon string `json:"icon,omitempty"`
	// Tags are short lowercase labels for finding and grouping it, also not versioned.
	Tags []string `json:"tags,omitempty"`
	// Owner is the person who created it; empty when an agent or the API did, and then
	// everyone counts as its owner. Capabilities is what the artifact declares, as
	// Claude's do: today db rules (capabilities.db.rules) take effect.
	Owner        string          `json:"owner,omitempty"`
	Capabilities json.RawMessage `json:"capabilities,omitempty"`
	Pinned       bool            `json:"pinned,omitempty"`
	// ArchivedAt is when it was archived, zero while it is in the library. Archived
	// artifacts keep everything, but listings and search leave them out unless asked, and
	// only they can be deleted in bulk.
	ArchivedAt time.Time  `json:"archived_at,omitzero"`
	Revisions  []Revision `json:"revisions"`
}
type Summary struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Kind        string    `json:"kind"`
	Version     int       `json:"version"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Description string    `json:"description,omitempty"`
	Icon        string    `json:"icon,omitempty"`
	Tags        []string  `json:"tags,omitempty"`
	Pinned      bool      `json:"pinned,omitempty"`
	ArchivedAt  time.Time `json:"archived_at,omitzero"`
	// OpenComments counts unresolved comment threads; Comments counts every comment.
	OpenComments int `json:"open_comments,omitempty"`
	Comments     int `json:"comments,omitempty"`
}
type Input struct {
	Title   string `json:"title"`
	Kind    string `json:"kind"`
	Content string `json:"content"`
	// Edits, when present (even empty), derive the new content from the latest revision
	// instead of Content, so a caller changes a few lines without resending the source.
	Edits []Edit `json:"edits,omitempty"`
	// Files adds or replaces supporting files; a null value removes one, and an update
	// keeps files it does not name, as Claude's artifact files do.
	Files map[string]*string `json:"files,omitempty"`
	Note  string             `json:"note,omitempty"`
	// Description, Icon and Tags replace the artifact's when present; nil keeps them.
	Description *string   `json:"description,omitempty"`
	Icon        *string   `json:"icon,omitempty"`
	Tags        *[]string `json:"tags,omitempty"`
	// Capabilities replaces the declared capabilities when present; JSON null clears them.
	Capabilities json.RawMessage `json:"capabilities,omitempty"`
	// Owner is set by the server from the person creating it, never by the client.
	Owner       string `json:"-"`
	BaseVersion int    `json:"base_version,omitempty"`
}

// Edit replaces OldStr, which must occur exactly once in the content, with NewStr. File
// names a supporting file to edit instead of the main source.
type Edit struct {
	OldStr string `json:"old_str"`
	NewStr string `json:"new_str"`
	File   string `json:"file,omitempty" jsonschema:"Supporting file to edit; omit for the main source"`
}

// ApplyEdits applies edits in order, all or none, to the main content or the file each
// names (files is changed in place). Each old_str must match exactly once in its target
// as it stands after the edits before it.
func ApplyEdits(content string, files map[string]string, edits []Edit) (string, error) {
	for i, e := range edits {
		target := content
		if e.File != "" {
			var ok bool
			if target, ok = files[e.File]; !ok {
				return "", fmt.Errorf("edit %d: file %q does not exist", i+1, e.File)
			}
		}
		if e.OldStr == "" {
			return "", fmt.Errorf("edit %d: old_str is empty", i+1)
		}
		switch n := strings.Count(target, e.OldStr); n {
		case 1:
			target = strings.Replace(target, e.OldStr, e.NewStr, 1)
		case 0:
			return "", fmt.Errorf("edit %d: old_str not found; copy it exactly from the latest source", i+1)
		default:
			return "", fmt.Errorf("edit %d: old_str occurs %d times; include more surrounding text so it is unique", i+1, n)
		}
		if e.File != "" {
			files[e.File] = target
		} else {
			content = target
		}
	}
	return content, nil
}

// mergeFiles applies an update's file changes to a copy of the previous files.
func mergeFiles(previous map[string]string, changes map[string]*string) map[string]string {
	out := map[string]string{}
	for k, v := range previous {
		out[k] = v
	}
	for k, v := range changes {
		if v == nil {
			delete(out, k)
		} else {
			out[k] = *v
		}
	}
	return out
}

// ValidateFiles checks supporting files' paths and sizes.
func ValidateFiles(content string, files map[string]string) error {
	if len(files) > MaxFiles {
		return fmt.Errorf("an artifact holds at most %d files", MaxFiles)
	}
	total := len(content)
	for path, body := range files {
		if len(path) > maxFilePath || !filePath.MatchString(path) {
			return fmt.Errorf("file path %q must be relative, like style.css or lib/app.js (letters, digits, . _ -)", path)
		}
		if len(body) > MaxContent {
			return fmt.Errorf("file %s exceeds 2 MiB", path)
		}
		total += len(body)
	}
	if total > MaxRevision {
		return errors.New("source and files exceed 8 MiB together")
	}
	return nil
}

type Store struct {
	// KeepVersions is how many of each app's newest ready versions keep their files (0:
	// DefaultKeepVersions). Set it before serving.
	KeepVersions int

	mu      sync.Mutex
	dir     string
	items   map[string]Artifact
	uploads map[string]MediaUpload
	// writing holds a lock per artifact, guarded by mu, that serializes the writes to that
	// artifact's file. A save encodes and syncs the file holding only it, so a large
	// artifact's history never holds up every other request behind mu. It is taken before
	// mu, never while holding it.
	writing map[string]*sync.Mutex
	// comments is guarded by mu, like items.
	comments map[string][]Comment
	// kvMu guards artifact storage; it may take mu, never the reverse.
	kvMu sync.Mutex
	kv   map[string]*kv
	// people, shares, assets and assetOwner are guarded by mu.
	people     map[string]Person
	shares     map[string]Share
	assets     map[string][]Asset
	assetOwner map[string]string
	// dbMu guards the document databases; it may take mu, never the reverse.
	dbMu     sync.Mutex
	dbs      map[string]*docDB
	onChange ChangeFunc
	// apps, appShares (token to app id), appLabels (hostname label, current or former, to
	// app id) and appStates are guarded by mu.
	apps      map[string]App
	appShares map[string]string
	appLabels map[string]string
	appStates map[string]*appState
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, items: map[string]Artifact{}, writing: map[string]*sync.Mutex{}}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		b, e := os.ReadFile(f)
		if e != nil {
			return nil, e
		}
		var a Artifact
		if e = json.Unmarshal(b, &a); e != nil {
			return nil, fmt.Errorf("read %s: %w", f, e)
		}
		if a.ID == "" || filepath.Base(f) != a.ID+".json" || len(a.Revisions) == 0 {
			return nil, fmt.Errorf("invalid artifact file %s", f)
		}
		for i, r := range a.Revisions {
			if r.Version != i+1 {
				return nil, fmt.Errorf("invalid revision in %s", f)
			}
			if e = validate(Input{Title: r.Title, Kind: r.Kind, Content: r.Content, Note: r.Note}); e != nil {
				return nil, fmt.Errorf("invalid revision in %s: %w", f, e)
			}
			if e = ValidateFiles(r.Content, r.Files); e != nil {
				return nil, fmt.Errorf("invalid revision in %s: %w", f, e)
			}
		}
		s.items[a.ID] = a
	}
	if err = s.loadComments(); err != nil {
		return nil, err
	}
	if err = s.loadPeople(); err != nil {
		return nil, err
	}
	if err = s.loadAssets(); err != nil {
		return nil, err
	}
	if err = s.loadShares(); err != nil {
		return nil, err
	}
	if err = s.loadApps(); err != nil {
		return nil, err
	}
	// Upload sessions do not survive restarts; remove their abandoned temporary bytes.
	for _, pattern := range []string{filepath.Join(dir, ".media-*"), filepath.Join(dir, "media", ".upload-*")} {
		pending, err := filepath.Glob(pattern)
		if err != nil {
			return nil, err
		}
		for _, path := range pending {
			if err := os.Remove(path); err != nil {
				return nil, fmt.Errorf("clean interrupted media upload: %w", err)
			}
		}
	}
	return s, nil
}
func validate(in Input) error {
	if strings.TrimSpace(in.Title) == "" || len(in.Title) > 200 {
		return errors.New("title must contain 1–200 bytes")
	}
	ok := false
	for _, k := range Kinds {
		if in.Kind == k {
			ok = true
		}
	}
	if !ok {
		return errors.New("kind must be html, react, markdown, slides, svg, mermaid, or code")
	}
	if len(in.Content) > MaxContent {
		return errors.New("content exceeds 2 MiB")
	}
	if len(in.Note) > 1000 {
		return errors.New("note exceeds 1000 bytes")
	}
	if in.Description != nil && len(*in.Description) > 300 {
		return errors.New("description exceeds 300 bytes")
	}
	if in.Icon != nil && (len(*in.Icon) > 32 || strings.ContainsAny(*in.Icon, "<>\"'&")) {
		return errors.New("icon must be one short word or an emoji")
	}
	if in.Capabilities != nil {
		var c any
		if err := json.Unmarshal(in.Capabilities, &c); err != nil || len(in.Capabilities) > 16<<10 {
			return errors.New("capabilities must be a JSON object of at most 16 KiB")
		}
		if _, isObject := c.(map[string]any); !isObject && c != nil {
			return errors.New("capabilities must be a JSON object")
		}
	}
	return nil
}

// MaxTags is how many tags an artifact keeps.
const MaxTags = 20

// NormalizeTags lowercases tags, drops a leading #, joins words with hyphens and removes
// blanks and repeats, keeping their order.
func NormalizeTags(in []string) ([]string, error) {
	out := []string{}
	for _, t := range in {
		t = strings.Join(strings.Fields(strings.ToLower(strings.TrimLeft(strings.TrimSpace(t), "#"))), "-")
		switch {
		case t == "" || slices.Contains(out, t):
			continue
		case len(t) > 32 || strings.ContainsAny(t, "<>\"'&,:#"):
			return nil, fmt.Errorf("tag %q must be one short label of at most 32 bytes, without punctuation like , : < >", t)
		}
		out = append(out, t)
	}
	if len(out) > MaxTags {
		return nil, fmt.Errorf("an artifact holds at most %d tags", MaxTags)
	}
	return out, nil
}

// ArchiveFilter says which artifacts a listing or search includes.
type ArchiveFilter string

const (
	// ExcludeArchived is the library: everything not archived. It is the default.
	ExcludeArchived ArchiveFilter = ""
	OnlyArchived    ArchiveFilter = "only"
	IncludeArchived ArchiveFilter = "include"
)

// ParseArchiveFilter reads exclude (or nothing), only or include.
func ParseArchiveFilter(v string) (ArchiveFilter, error) {
	switch f := ArchiveFilter(strings.ToLower(strings.TrimSpace(v))); f {
	case "exclude":
		return ExcludeArchived, nil
	case ExcludeArchived, OnlyArchived, IncludeArchived:
		return f, nil
	}
	return "", fmt.Errorf("archived must be exclude, include or only, not %q", v)
}

// Includes reports whether a listing with this filter shows an artifact.
func (f ArchiveFilter) Includes(archived bool) bool {
	switch f {
	case OnlyArchived:
		return archived
	case IncludeArchived:
		return true
	}
	return !archived
}

// List returns the artifacts the filter includes, most recently updated first.
func (s *Store) List(f ArchiveFilter) []Summary {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Summary, 0, len(s.items))
	for _, a := range s.items {
		if f.Includes(!a.ArchivedAt.IsZero()) {
			out = append(out, s.summary(a))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out
}

// summary describes a for the library. s.mu is held.
func (s *Store) summary(a Artifact) Summary {
	r := a.Revisions[len(a.Revisions)-1]
	return Summary{ID: a.ID, Title: r.Title, Kind: r.Kind, Version: r.Version, CreatedAt: a.CreatedAt, UpdatedAt: r.CreatedAt,
		Description: a.Description, Icon: a.Icon, Tags: a.Tags, Pinned: a.Pinned, ArchivedAt: a.ArchivedAt, OpenComments: s.openThreads(a.ID), Comments: len(s.comments[a.ID])}
}
func (s *Store) Get(id string) (Artifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.items[id]
	if !ok {
		return Artifact{}, ErrNotFound
	}
	a.Revisions = append([]Revision(nil), a.Revisions...)
	return a, nil
}
func (s *Store) Save(id string, in Input) (Artifact, error) {
	a, err := s.save(id, in)
	if err == nil {
		s.changed(a.ID, "version", strconv.Itoa(len(a.Revisions)))
	}
	return a, err
}
func (s *Store) save(id string, in Input) (Artifact, error) {
	var files map[string]string
	if in.Tags != nil {
		tags, err := NormalizeTags(*in.Tags)
		if err != nil {
			return Artifact{}, err
		}
		in.Tags = &tags
	}
	if id == "" {
		if in.Edits != nil {
			return Artifact{}, errors.New("edits need an existing artifact; send content to create one")
		}
		for path, v := range in.Files {
			if v == nil {
				return Artifact{}, fmt.Errorf("file %s: a new artifact's files need content", path)
			}
		}
		files = mergeFiles(nil, in.Files)
		if err := validate(in); err != nil {
			return Artifact{}, err
		}
		if err := ValidateFiles(in.Content, files); err != nil {
			return Artifact{}, err
		}
	}
	var a Artifact
	if id == "" {
		b := make([]byte, 12)
		if _, e := rand.Read(b); e != nil {
			return a, e
		}
		a = Artifact{ID: hex.EncodeToString(b), CreatedAt: time.Now().UTC()}
	} else {
		// Holding the artifact's write lock, nothing else changes it, so the new revision is
		// built and written from this copy without holding mu.
		defer s.lockWrites(id)()
		var ok bool
		s.mu.Lock()
		a, ok = s.items[id]
		s.mu.Unlock()
		if !ok {
			return a, ErrNotFound
		}
		if in.BaseVersion != len(a.Revisions) {
			return Artifact{}, ErrConflict
		}
		// An update may omit the title or type to keep them.
		latest := a.Revisions[len(a.Revisions)-1]
		if strings.TrimSpace(in.Title) == "" {
			in.Title = latest.Title
		}
		if in.Kind == "" {
			in.Kind = latest.Kind
		}
		files = mergeFiles(latest.Files, in.Files)
		if in.Edits != nil {
			if in.Content != "" {
				return Artifact{}, errors.New("send content or edits, not both")
			}
			content, err := ApplyEdits(latest.Content, files, in.Edits)
			if err != nil {
				return Artifact{}, err
			}
			in.Content = content
		}
		if err := validate(in); err != nil {
			return Artifact{}, err
		}
		if err := ValidateFiles(in.Content, files); err != nil {
			return Artifact{}, err
		}
	}
	if len(files) == 0 {
		files = nil
	}
	a.Revisions = append(append([]Revision(nil), a.Revisions...), Revision{Version: len(a.Revisions) + 1, Title: strings.TrimSpace(in.Title), Kind: in.Kind, Content: in.Content, Files: files, Note: in.Note, CreatedAt: time.Now().UTC()})
	if in.Description != nil {
		a.Description = strings.TrimSpace(*in.Description)
	}
	if in.Icon != nil {
		a.Icon = strings.ToLower(strings.TrimSpace(*in.Icon))
	}
	if in.Tags != nil {
		a.Tags = *in.Tags
		if len(a.Tags) == 0 {
			a.Tags = nil
		}
	}
	if in.Capabilities != nil {
		if string(in.Capabilities) == "null" {
			a.Capabilities = nil
		} else {
			a.Capabilities = in.Capabilities
		}
	}
	if len(a.Revisions) == 1 {
		a.Owner = in.Owner
	}
	return s.write(a)
}

// SetPinned pins or unpins an artifact without adding a revision.
func (s *Store) SetPinned(id string, pinned bool) (Artifact, error) {
	defer s.lockWrites(id)()
	s.mu.Lock()
	a, ok := s.items[id]
	s.mu.Unlock()
	if !ok {
		return Artifact{}, ErrNotFound
	}
	a.Pinned = pinned
	return s.write(a)
}

// SetArchived moves an artifact into the archive or back to the library, without adding a
// revision. Archiving an archived artifact keeps when it was first archived.
func (s *Store) SetArchived(id string, archived bool) (Artifact, error) {
	unlock := s.lockWrites(id)
	s.mu.Lock()
	a, ok := s.items[id]
	s.mu.Unlock()
	if !ok {
		unlock()
		return Artifact{}, ErrNotFound
	}
	if archived == !a.ArchivedAt.IsZero() {
		unlock()
		a.Revisions = append([]Revision(nil), a.Revisions...)
		return a, nil
	}
	a.ArchivedAt = time.Time{}
	if archived {
		a.ArchivedAt = time.Now().UTC()
	}
	a, err := s.write(a)
	unlock()
	if err == nil {
		s.changed(id, "archived", strconv.FormatBool(archived))
	}
	return a, err
}

// lockWrites takes the artifact's write lock and returns its unlock. s.mu is not held.
func (s *Store) lockWrites(id string) func() {
	s.mu.Lock()
	l := s.writing[id]
	if l == nil {
		l = &sync.Mutex{}
		s.writing[id] = l
	}
	s.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// write persists a, then publishes it, and returns a copy the caller may change. The caller
// holds the artifact's write lock (or a is new and unseen), and not s.mu: encoding and
// syncing the file grow with the artifact's history.
func (s *Store) write(a Artifact) (Artifact, error) {
	b, err := encodeArtifact(a)
	if err != nil {
		return Artifact{}, err
	}
	if err = writeAtomic(s.dir, filepath.Join(s.dir, a.ID+".json"), b); err != nil {
		return Artifact{}, err
	}
	s.mu.Lock()
	s.items[a.ID] = a
	s.mu.Unlock()
	a.Revisions = append([]Revision(nil), a.Revisions...)
	return a, nil
}

// encodeArtifact is an artifact file: compact, and with < > & written as they are rather
// than as \u003c-style escapes, which make an HTML artifact's history a third larger on
// disk and slower to write. Files written either way read the same.
func encodeArtifact(a Artifact) ([]byte, error) {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(a); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Delete permanently removes an artifact whose latest version is base, with its comments,
// assets, shares, data and storage.
func (s *Store) Delete(id string, base int) error {
	return s.remove(id, func(a Artifact) error {
		if base != len(a.Revisions) {
			return ErrConflict
		}
		return nil
	})
}

// DeleteArchived is Delete for an artifact that is in the archive, whatever its version:
// one restored meanwhile is refused with ErrNotArchived.
func (s *Store) DeleteArchived(id string) error {
	return s.remove(id, func(a Artifact) error {
		if a.ArchivedAt.IsZero() {
			return ErrNotArchived
		}
		return nil
	})
}

// remove deletes an artifact once check, run holding its write lock, allows it.
func (s *Store) remove(id string, check func(Artifact) error) error {
	defer s.lockWrites(id)()
	s.mu.Lock()
	a, ok := s.items[id]
	if !ok {
		s.mu.Unlock()
		return ErrNotFound
	}
	if err := check(a); err != nil {
		s.mu.Unlock()
		return err
	}
	if e := os.Remove(filepath.Join(s.dir, id+".json")); e != nil {
		s.mu.Unlock()
		return e
	}
	delete(s.items, id)
	// Anyone already waiting on the write lock finds the artifact gone.
	delete(s.writing, id)
	delete(s.comments, id)
	err := os.Remove(s.commentsPath(id))
	if assetErr := s.dropAssets(id); err == nil || os.IsNotExist(err) {
		err = assetErr
	}
	if shareErr := s.dropShares(id); err == nil {
		err = shareErr
	}
	s.mu.Unlock()
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err = s.dropDB(id); err != nil {
		return err
	}
	s.changed(id, "deleted", "")
	return s.dropStorage(id)
}

// writeAtomic replaces path via a synced temporary file in dir and a rename.
func writeAtomic(dir, path string, b []byte) error {
	f, err := os.CreateTemp(dir, ".save-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
