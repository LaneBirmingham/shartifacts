package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const MaxMedia = 256 << 20
const MediaChunkSize = 1 << 20

// The extension is selected by the server, never by a client-supplied path.
// Data files (CSV, JSON, text, spreadsheets, PDF) are what artifacts read with
// window.fs.readFile; they are never rendered as pages.
var mediaTypes = map[string]string{"video/mp4": ".mp4", "video/webm": ".webm", "video/ogg": ".ogv", "video/quicktime": ".mov", "audio/mpeg": ".mp3", "audio/mp4": ".m4a", "audio/ogg": ".ogg", "audio/wav": ".wav", "audio/webm": ".weba", "audio/flac": ".flac", "image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp", "image/avif": ".avif", "text/vtt": ".vtt",
	"text/csv": ".csv", "application/json": ".json", "text/plain": ".txt", "text/markdown": ".md", "application/pdf": ".pdf", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": ".xlsx"}

type Media struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	MIMEType string `json:"mime_type"`
	Size     int64  `json:"size"`
	// Name is the file name it was last uploaded under, for window.fs.readFile.
	Name       string     `json:"name,omitempty"`
	UploadedAt *time.Time `json:"uploaded_at,omitempty"`
}

func (s *Store) namesPath() string { return filepath.Join(s.dir, "media", "names.json") }

// mediaNames reads the file-name index. s.mu is held.
func (s *Store) mediaNames() (map[string]string, error) {
	names := map[string]string{}
	b, err := os.ReadFile(s.namesPath())
	if os.IsNotExist(err) {
		return names, nil
	}
	if err == nil {
		err = json.Unmarshal(b, &names)
	}
	return names, err
}

// NameMedia records the file name an upload arrived with.
func (s *Store) NameMedia(m Media, name string) (Media, error) {
	name = filepath.Base(strings.TrimSpace(strings.ReplaceAll(name, "\\", "/")))
	if name == "." || name == "/" || name == "" {
		return m, nil
	}
	if len(name) > 255 || strings.ContainsFunc(name, func(r rune) bool { return r < 32 || r == 127 }) {
		return m, errors.New("file name must be 1–255 bytes without control characters")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	names, err := s.mediaNames()
	if err != nil {
		return m, err
	}
	names[m.ID] = name
	b, err := json.Marshal(names)
	if err != nil {
		return m, err
	}
	if err = writeAtomic(filepath.Join(s.dir, "media"), s.namesPath(), b); err != nil {
		return m, err
	}
	m.Name = name
	return m, nil
}

type MediaUpload struct {
	ID        string `json:"upload_id"`
	Offset    int64  `json:"offset"`
	ChunkSize int    `json:"max_chunk_bytes"`
	mime      string
	expires   time.Time
}

func mediaType(raw string) (string, string, error) {
	t, _, err := mime.ParseMediaType(raw)
	ext := mediaTypes[t]
	if err != nil || ext == "" {
		return "", "", errors.New("unsupported media type; use MP4/WebM video, common audio/raster images, text/vtt captions, or CSV/JSON/text/Markdown/XLSX/PDF data")
	}
	return t, ext, nil
}
func (s *Store) SaveMedia(raw string, src io.Reader) (Media, error) {
	t, ext, err := mediaType(raw)
	if err != nil {
		return Media{}, err
	}
	dir := filepath.Join(s.dir, "media")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return Media{}, err
	}
	f, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return Media{}, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(src, MaxMedia+1))
	if err != nil {
		return Media{}, err
	}
	if n == 0 || n > MaxMedia {
		return Media{}, errors.New("media must contain 1–268435456 bytes (maximum 256 MiB)")
	}
	if err = f.Sync(); err != nil {
		return Media{}, err
	}
	if err = f.Close(); err != nil {
		return Media{}, err
	}
	id := hex.EncodeToString(h.Sum(nil)) + ext
	if err = os.Rename(f.Name(), filepath.Join(dir, id)); err != nil {
		return Media{}, err
	}
	return Media{ID: id, URL: "/media/" + id, MIMEType: t, Size: n}, nil
}
func (s *Store) MediaPath(id string) (string, string, error) {
	ext := filepath.Ext(id)
	hash := strings.TrimSuffix(id, ext)
	b, err := hex.DecodeString(hash)
	if err != nil || len(b) != 32 || hex.EncodeToString(b) != hash {
		return "", "", ErrNotFound
	}
	for t, e := range mediaTypes {
		if e == ext {
			return filepath.Join(s.dir, "media", id), t, nil
		}
	}
	return "", "", ErrNotFound
}
func (s *Store) ListMedia() ([]Media, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "media"))
	if os.IsNotExist(err) {
		return []Media{}, nil
	}
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	names, err := s.mediaNames()
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out := []Media{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		_, t, err := s.MediaPath(e.Name())
		if err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		uploaded := info.ModTime().UTC()
		out = append(out, Media{e.Name(), "/media/" + e.Name(), t, info.Size(), names[e.Name()], &uploaded})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func (s *Store) BeginMediaUpload(raw string) (MediaUpload, error) {
	t, _, err := mediaType(raw)
	if err != nil {
		return MediaUpload{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.uploads == nil {
		s.uploads = map[string]MediaUpload{}
	}
	for id, u := range s.uploads {
		if time.Now().After(u.expires) {
			os.Remove(filepath.Join(s.dir, ".media-"+id))
			delete(s.uploads, id)
		}
	}
	if len(s.uploads) >= 16 {
		return MediaUpload{}, errors.New("upload capacity reached; finish pending uploads or wait 30 minutes")
	}
	b := make([]byte, 16)
	if _, err = rand.Read(b); err != nil {
		return MediaUpload{}, err
	}
	id := hex.EncodeToString(b)
	f, err := os.OpenFile(filepath.Join(s.dir, ".media-"+id), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return MediaUpload{}, err
	}
	if err = f.Close(); err != nil {
		return MediaUpload{}, err
	}
	u := MediaUpload{ID: id, ChunkSize: MediaChunkSize, mime: t, expires: time.Now().Add(30 * time.Minute)}
	s.uploads[id] = u
	return u, nil
}
func (s *Store) AppendMedia(id string, offset int64, data []byte, final bool) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.uploads[id]
	if !ok {
		return nil, errors.New("upload not found; begin a new upload")
	}
	path := filepath.Join(s.dir, ".media-"+id)
	if time.Now().After(u.expires) {
		os.Remove(path)
		delete(s.uploads, id)
		return nil, errors.New("upload expired")
	}
	if offset != u.Offset {
		return nil, fmt.Errorf("upload offset conflict: expected %d", u.Offset)
	}
	if len(data) > MediaChunkSize || (len(data) == 0 && !final) {
		return nil, errors.New("chunk must contain 1–1048576 decoded bytes, or an empty final chunk")
	}
	if offset+int64(len(data)) > MaxMedia {
		return nil, errors.New("media exceeds 256 MiB")
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	_, err = f.WriteAt(data, offset)
	closeErr := f.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	u.Offset += int64(len(data))
	u.expires = time.Now().Add(30 * time.Minute)
	s.uploads[id] = u
	if !final {
		return u, nil
	}
	src, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	media, err := s.SaveMedia(u.mime, src)
	if err != nil {
		return nil, err
	}
	os.Remove(path)
	delete(s.uploads, id)
	return media, nil
}

// ErrMediaNotFound is a Media library file that is not there.
var ErrMediaNotFound = notFoundError("media not found")

// DeleteMedia removes a file from the Media library, with its name. Pages that embed it
// show it as missing from then on; MediaUsage says which those are.
func (s *Store) DeleteMedia(id string) error {
	path, _, err := s.MediaPath(id)
	if err != nil {
		return ErrMediaNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return ErrMediaNotFound
		}
		return err
	}
	names, err := s.mediaNames()
	if err != nil {
		return err
	}
	if _, ok := names[id]; !ok {
		return nil
	}
	delete(names, id)
	b, err := json.Marshal(names)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.dir, "media"), s.namesPath(), b)
}

// A MediaUser is an artifact or app whose source mentions a Media library file.
type MediaUser struct {
	Kind  string `json:"kind"` // artifact or app
	ID    string `json:"id"`
	Title string `json:"title"`
}

// maxScannedAppFile bounds the app source files MediaUsage reads; larger ones are data or
// bundles of libraries rather than code that names media.
const maxScannedAppFile = 4 << 20

// appTextFile reports whether an app source file is text that could name media.
func appTextFile(p string) bool {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".html", ".htm", ".js", ".mjs", ".cjs", ".jsx", ".ts", ".tsx", ".css", ".json", ".md", ".txt", ".svg", ".go", ".rs", ".vtt", ".csv":
		return true
	}
	return false
}

// MediaUsage finds who uses each of media: artifacts whose source or supporting files, in
// any revision (restoring one would bring the file back into use), or whose saved data (db
// and storage) mention its ID, or its file name as a quoted string as window.fs.readFile
// takes it; and apps whose kept versions' text files do. App backends' data and pages that
// choose files while they run go unseen, so a file nobody uses is one nothing names, not one
// nothing could load.
func (s *Store) MediaUsage(media []Media) (map[string][]MediaUser, error) {
	ids, names := map[string]bool{}, map[string][]string{}
	for _, m := range media {
		ids[m.ID] = true
		if m.Name != "" {
			names[m.Name] = append(names[m.Name], m.ID)
		}
	}
	out := map[string][]MediaUser{}
	record := func(u MediaUser) func(string) {
		return func(id string) {
			for _, have := range out[id] {
				if have.Kind == u.Kind && have.ID == u.ID {
					return
				}
			}
			out[id] = append(out[id], u)
		}
	}
	// Read the library under the lock, and its text without it: revisions never change.
	type source struct {
		user      MediaUser
		revisions []Revision
	}
	s.mu.Lock()
	sources := make([]source, 0, len(s.items))
	for _, a := range s.items {
		sources = append(sources, source{MediaUser{"artifact", a.ID, a.Revisions[len(a.Revisions)-1].Title}, a.Revisions})
	}
	s.mu.Unlock()
	for _, src := range sources {
		found := record(src.user)
		for _, r := range src.revisions {
			scanMediaRefs(r.Content, ids, names, found)
			for _, body := range r.Files {
				scanMediaRefs(body, ids, names, found)
			}
		}
		// Saved data is replaced whole by rename, so reading it without the lock sees one
		// version or the next.
		for _, path := range []string{s.dbPath(src.user.ID), s.storagePath(src.user.ID)} {
			if b, err := os.ReadFile(path); err == nil {
				scanMediaRefs(string(b), ids, names, found)
			}
		}
	}
	seen := map[fileID]bool{}
	for _, summary := range s.Apps() {
		a, err := s.App(summary.ID)
		if err != nil {
			continue // deleted meanwhile
		}
		found := record(MediaUser{"app", a.ID, a.Name})
		for _, d := range a.Deployments {
			if d.Pruned {
				continue
			}
			root := s.AppSourceDir(a.ID, d.Version)
			err := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
				if err != nil || !e.Type().IsRegular() || !appTextFile(path) {
					return nil // a version pruned while we read it, or not text
				}
				info, err := e.Info()
				if err != nil || info.Size() > maxScannedAppFile {
					return nil
				}
				if fid, ok := identify(info); ok {
					if seen[fid] {
						return nil // the same file, linked into another version
					}
					seen[fid] = true
				}
				b, err := os.ReadFile(path)
				if err == nil {
					scanMediaRefs(string(b), ids, names, found)
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// scanMediaRefs calls found for each media ID text mentions (64 hex digits, a dot and an
// extension, as in /media/ID.png) and for each media file whose name text holds as a
// quoted string. Every quote is tried as an opening one, as an apostrophe in prose would
// otherwise pair with the quote before a name: a use found where there is none only keeps
// a file, while one missed would offer it for deletion.
func scanMediaRefs(text string, ids map[string]bool, names map[string][]string, found func(id string)) {
	run := 0
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c >= '0' && c <= '9' || c >= 'a' && c <= 'f' {
			run++
			continue
		}
		if c == '.' && run >= 64 {
			end := i + 1
			for end < len(text) && end-i <= 4 && (text[end] >= 'a' && text[end] <= 'z' || text[end] >= '0' && text[end] <= '9') {
				end++
			}
			for k := end; k > i+1; k-- {
				if id := text[i-64 : k]; ids[id] {
					found(id)
					break
				}
			}
		}
		run = 0
	}
	if len(names) == 0 {
		return
	}
	for i := 0; i < len(text); i++ {
		q := text[i]
		if q != '"' && q != '\'' && q != '`' {
			continue
		}
		end := strings.IndexByte(text[i+1:min(len(text), i+2+255)], q)
		if end < 0 {
			continue
		}
		for _, id := range names[text[i+1:i+1+end]] {
			found(id)
		}
	}
}
