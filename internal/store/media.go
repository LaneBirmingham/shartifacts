package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
