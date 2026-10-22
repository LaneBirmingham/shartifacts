package store

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"time"
	"unicode/utf8"
)

// Assets are files an artifact's page uploads for itself (Claude's `assets`): photos on
// a team wall, a data file to parse, a font. They belong to the artifact, are served at
// /_blob/<id>, and are deleted with it. The Media library is the shared, global store.

var (
	ErrUnsupportedType = errors.New("unsupported_type")
	ErrTooLarge        = errors.New("too_large")
)

const (
	MaxAssets      = 1000
	MaxAssetsBytes = 1 << 30
)

var assetTypes = map[string]int64{
	"image/png": 20 << 20, "image/jpeg": 20 << 20, "image/gif": 20 << 20, "image/webp": 20 << 20, "image/svg+xml": 2 << 20,
	"video/mp4": 20 << 20, "video/webm": 20 << 20, "application/pdf": 20 << 20,
	"font/woff2": 20 << 20, "font/woff": 20 << 20, "font/ttf": 20 << 20, "font/otf": 20 << 20,
	"text/csv": 20 << 20, "text/markdown": 20 << 20, "application/json": 20 << 20, "text/plain": 20 << 20,
	"text/css": 16 << 20, "text/javascript": 16 << 20,
}

type Asset struct {
	ID          string    `json:"id"`
	URL         string    `json:"url"`
	ContentType string    `json:"contentType"`
	SizeBytes   int64     `json:"sizeBytes"`
	CreatedAt   time.Time `json:"createdAt"`
	Uploader    string    `json:"uploader,omitempty"`
}

type AssetUsage struct {
	Files    int   `json:"files"`
	Bytes    int64 `json:"bytes"`
	MaxFiles int   `json:"maxFiles"`
	MaxBytes int64 `json:"maxBytes"`
}

func (s *Store) assetDir(artifact string) string { return filepath.Join(s.dir, "assets", artifact) }

func (s *Store) loadAssets() error {
	s.assets = map[string][]Asset{}
	s.assetOwner = map[string]string{}
	dirs, err := os.ReadDir(filepath.Join(s.dir, "assets"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, d := range dirs {
		if _, ok := s.items[d.Name()]; !ok || !d.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.assetDir(d.Name()), "index.json"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		var list []Asset
		if err = json.Unmarshal(b, &list); err != nil {
			return fmt.Errorf("read assets of %s: %w", d.Name(), err)
		}
		s.assets[d.Name()] = list
		for _, a := range list {
			s.assetOwner[a.ID] = d.Name()
		}
	}
	return nil
}

// UploadAsset stores body as an asset of the artifact under contentType.
func (s *Store) UploadAsset(artifact, contentType, uploader string, body io.Reader) (Asset, error) {
	t, params, err := mime.ParseMediaType(contentType)
	limit, ok := assetTypes[t]
	if err != nil || !ok || len(params) > 0 || t != contentType {
		return Asset{}, fmt.Errorf("%w: %q is not an accepted type (exact media types such as image/png or text/csv)", ErrUnsupportedType, contentType)
	}
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return Asset{}, err
	}
	if len(data) == 0 {
		return Asset{}, fmt.Errorf("%w: the file is empty", ErrInvalidArgument)
	}
	if int64(len(data)) > limit {
		return Asset{}, fmt.Errorf("%w: %s files are at most %d MiB", ErrTooLarge, t, limit>>20)
	}
	first := bytes.TrimLeft(data, " \t\r\n\uFEFF")
	markup := len(first) > 0 && first[0] == '<'
	switch t {
	case "image/svg+xml":
		if !bytes.Contains(data[:min(len(data), 4096)], []byte("<svg")) {
			return Asset{}, fmt.Errorf("%w: not an SVG document", ErrUnsupportedType)
		}
	case "text/csv", "text/markdown", "application/json", "text/plain":
		if !utf8.Valid(bytes.TrimPrefix(data, []byte("\uFEFF"))) {
			return Asset{}, fmt.Errorf("%w: text files must be UTF-8", ErrInvalidArgument)
		}
	case "text/css", "text/javascript":
		if markup || !utf8.Valid(data) {
			return Asset{}, fmt.Errorf("%w: a stylesheet or script must be text, not markup", ErrUnsupportedType)
		}
	default:
		if markup {
			return Asset{}, fmt.Errorf("%w: markup uploads only as image/svg+xml", ErrUnsupportedType)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[artifact]; !ok {
		return Asset{}, ErrNotFound
	}
	list := s.assets[artifact]
	var used int64
	for _, a := range list {
		used += a.SizeBytes
	}
	if len(list) >= MaxAssets || used+int64(len(data)) > MaxAssetsBytes {
		return Asset{}, fmt.Errorf("%w: this artifact's asset storage is full", ErrQuotaExceeded)
	}
	b := make([]byte, 16)
	if _, err = rand.Read(b); err != nil {
		return Asset{}, err
	}
	id := hex.EncodeToString(b)
	dir := s.assetDir(artifact)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return Asset{}, err
	}
	if err = writeAtomic(dir, filepath.Join(dir, id), data); err != nil {
		return Asset{}, err
	}
	a := Asset{ID: id, URL: "/_blob/" + id, ContentType: t, SizeBytes: int64(len(data)), CreatedAt: time.Now().UTC(), Uploader: uploader}
	if err = s.writeAssetIndex(artifact, append(append([]Asset{}, list...), a)); err != nil {
		os.Remove(filepath.Join(dir, id))
		return Asset{}, err
	}
	s.assetOwner[id] = artifact
	return a, nil
}

// writeAssetIndex persists an artifact's asset list. s.mu is held.
func (s *Store) writeAssetIndex(artifact string, list []Asset) error {
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err = writeAtomic(s.assetDir(artifact), filepath.Join(s.assetDir(artifact), "index.json"), b); err != nil {
		return err
	}
	s.assets[artifact] = list
	return nil
}

func (s *Store) Assets(artifact string) ([]Asset, AssetUsage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[artifact]; !ok {
		return nil, AssetUsage{}, ErrNotFound
	}
	list := append([]Asset{}, s.assets[artifact]...)
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
	u := AssetUsage{Files: len(list), MaxFiles: MaxAssets, MaxBytes: MaxAssetsBytes}
	for _, a := range list {
		u.Bytes += a.SizeBytes
	}
	return list, u, nil
}

// DeleteAsset removes one asset by id; false when the artifact has no such asset.
func (s *Store) DeleteAsset(artifact, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[artifact]; !ok {
		return false, ErrNotFound
	}
	list := s.assets[artifact]
	for i, a := range list {
		if a.ID == id {
			if err := s.writeAssetIndex(artifact, append(append([]Asset{}, list[:i]...), list[i+1:]...)); err != nil {
				return false, err
			}
			delete(s.assetOwner, id)
			os.Remove(filepath.Join(s.assetDir(artifact), id))
			return true, nil
		}
	}
	return false, nil
}

// ArtifactAssetFile locates a stored asset only when it belongs to artifact.
func (s *Store) ArtifactAssetFile(artifact, id string) (string, string, error) {
	s.mu.Lock()
	owner, ok := s.assetOwner[id]
	s.mu.Unlock()
	if !ok || owner != artifact {
		return "", "", ErrNotFound
	}
	return s.AssetFile(id)
}

// AssetFile locates a stored asset for serving.
func (s *Store) AssetFile(id string) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	artifact, ok := s.assetOwner[id]
	if !ok {
		return "", "", ErrNotFound
	}
	for _, a := range s.assets[artifact] {
		if a.ID == id {
			return filepath.Join(s.assetDir(artifact), id), a.ContentType, nil
		}
	}
	return "", "", ErrNotFound
}

// dropAssets removes an artifact's assets with it. s.mu is held.
func (s *Store) dropAssets(artifact string) error {
	for _, a := range s.assets[artifact] {
		delete(s.assetOwner, a.ID)
	}
	delete(s.assets, artifact)
	if err := os.RemoveAll(s.assetDir(artifact)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
