package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
)

// FileKind is an artifact that is an uploaded file rather than source: a zip to hand to
// someone, say. The bytes live in the Media library, so they are streamed, kept out of the
// artifact's JSON and its 2 MiB source limit, and found by the unused-file check like any
// other reference. Its versions, comments and share links are those of any artifact.
const FileKind = "file"

// A FileRef is a file artifact's content, as JSON. Name is kept here rather than read from
// the Media library's name index because uploads are stored by their hash: uploading the
// same bytes under another name must not rename a file already handed out.
type FileRef struct {
	URL      string `json:"url"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	MIMEType string `json:"mime_type"`
}

// MediaID is the Media library file the reference names.
func (f FileRef) MediaID() string { return strings.TrimPrefix(f.URL, "/media/") }

var fileRefURL = regexp.MustCompile(`^/media/[0-9a-f]{64}\.[a-z0-9]+$`)

// ParseFileRef reads the content of a saved file artifact.
func ParseFileRef(content string) (FileRef, error) {
	var f FileRef
	if err := json.Unmarshal([]byte(content), &f); err != nil {
		return f, errors.New("a file artifact's content must name an uploaded file")
	}
	if !fileRefURL.MatchString(f.URL) {
		return f, errors.New("a file artifact's content must name an uploaded file by its /media/ URL")
	}
	if name, err := cleanFileName(f.Name); err != nil || name == "" || name != f.Name {
		return f, errors.New("a file artifact needs a file name of 1–255 bytes without control characters")
	}
	return f, nil
}

// fileContent turns what a caller sent as a file artifact's content into what is saved. It
// takes the /media/ URL an upload returned (or the whole URL, or the bare ID), or JSON with
// that url and a name, which is also how a duplicate or a restored version arrives. Size and
// type always come from the stored file, so a caller cannot make the download page misstate
// what visitors will get.
func (s *Store) fileContent(content, title string) (string, error) {
	var ref FileRef
	raw := strings.TrimSpace(content)
	if strings.HasPrefix(raw, "{") {
		if err := json.Unmarshal([]byte(raw), &ref); err != nil {
			return "", fmt.Errorf("file content: %w", err)
		}
		raw = ref.URL
	}
	id := raw
	if i := strings.LastIndex(raw, "/media/"); i >= 0 {
		id = raw[i+len("/media/"):]
	}
	file, typ, err := s.MediaPath(id)
	if err != nil {
		return "", errors.New("a file artifact's content must be the /media/ URL its upload returned")
	}
	info, err := os.Stat(file)
	if err != nil {
		return "", fmt.Errorf("no uploaded file %s: upload it first", id)
	}
	name, err := cleanFileName(ref.Name)
	if err != nil {
		return "", err
	}
	if name == "" {
		s.mu.Lock()
		names, err := s.mediaNames()
		s.mu.Unlock()
		if err != nil {
			return "", err
		}
		name = names[id]
	}
	if name == "" {
		// Uploaded without ?name=: the title is all there is, and it should still open as
		// what it is once downloaded.
		if name, err = cleanFileName(title); err != nil {
			return "", err
		}
		if ext := path.Ext(id); path.Ext(name) == "" && ext != ".bin" {
			name += ext
		}
	}
	b, err := json.Marshal(FileRef{URL: "/media/" + id, Name: name, Size: info.Size(), MIMEType: typ})
	if err != nil {
		return "", err
	}
	return string(b), nil
}
