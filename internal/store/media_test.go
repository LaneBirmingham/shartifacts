package store

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMediaUploadPersistence(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("test media bytes"), 100000)
	u, err := s.BeginMediaUpload("video/mp4")
	if err != nil {
		t.Fatal(err)
	}
	first := data[:MediaChunkSize]
	result, err := s.AppendMedia(u.ID, 0, first, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.(MediaUpload).Offset != MediaChunkSize {
		t.Fatal(result)
	}
	if _, err = s.AppendMedia(u.ID, 0, first, false); err == nil {
		t.Fatal("accepted stale offset")
	}
	result, err = s.AppendMedia(u.ID, MediaChunkSize, data[MediaChunkSize:], true)
	if err != nil {
		t.Fatal(err)
	}
	m := result.(Media)
	if m.Size != int64(len(data)) {
		t.Fatal(m)
	}
	if _, err = s.AppendMedia(u.ID, m.Size, nil, true); err == nil {
		t.Fatal("final upload reused")
	}
	duplicate, err := s.SaveMedia("video/mp4", bytes.NewReader(data))
	if err != nil || duplicate != m {
		t.Fatal(duplicate, err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	items, err := s.ListMedia()
	if err != nil || len(items) != 1 || items[0].UploadedAt == nil {
		t.Fatal(items, err)
	}
	if items[0].UploadedAt = nil; items[0] != m {
		t.Fatal(items, err)
	}
	path, typ, err := s.MediaPath(m.ID)
	if err != nil || typ != "video/mp4" {
		t.Fatal(path, typ, err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("media bytes changed", err)
	}
}
func TestMediaValidationAndCleanup(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, typ := range []string{"text/html", "image/svg+xml", "application/javascript", "video/nope", ""} {
		if _, err = s.SaveMedia(typ, strings.NewReader("data")); err == nil {
			t.Errorf("accepted %q", typ)
		}
		if _, err = s.BeginMediaUpload(typ); err == nil {
			t.Errorf("began %q", typ)
		}
	}
	if _, err = s.SaveMedia("image/png", strings.NewReader("")); err == nil {
		t.Fatal("accepted empty file")
	}
	for _, id := range []string{"../store.json", strings.Repeat("a", 64) + ".html", strings.Repeat("A", 64) + ".mp4", "x.mp4"} {
		if _, _, err = s.MediaPath(id); err == nil {
			t.Errorf("accepted path %s", id)
		}
	}
	u, _ := s.BeginMediaUpload("audio/mpeg")
	if _, err = s.AppendMedia(u.ID, 0, nil, false); err == nil {
		t.Fatal("empty nonfinal chunk")
	}
	if _, err = s.AppendMedia(u.ID, 0, make([]byte, MediaChunkSize+1), false); err == nil {
		t.Fatal("oversized chunk")
	}
	u.Offset = MaxMedia
	s.uploads[u.ID] = u
	if _, err = s.AppendMedia(u.ID, MaxMedia, []byte{1}, true); err == nil {
		t.Fatal("accepted oversized upload")
	}
	u.Offset = 0
	u.expires = time.Now().Add(-time.Second)
	s.uploads[u.ID] = u
	if _, err = s.AppendMedia(u.ID, 0, []byte{1}, true); err == nil {
		t.Fatal("accepted expired upload")
	}
	if _, err = os.Stat(filepath.Join(s.dir, ".media-"+u.ID)); !os.IsNotExist(err) {
		t.Fatal("expired upload remains", err)
	}
	u, _ = s.BeginMediaUpload("audio/mpeg")
	if _, err = s.AppendMedia(u.ID, 0, []byte{1}, false); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AppendMedia(u.ID, 1, nil, true); err != nil {
		t.Fatal("empty final chunk failed", err)
	}
}

type failingMediaReader struct{}

func (failingMediaReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func TestFailedMediaWriteCleansTemporaryFile(t *testing.T) {
	s, _ := Open(t.TempDir())
	if _, err := s.SaveMedia("video/mp4", failingMediaReader{}); err == nil {
		t.Fatal("accepted partial body")
	}
	entries, err := os.ReadDir(filepath.Join(s.dir, "media"))
	if err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
}

func TestMediaRestartDiscardsPendingUploads(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	u, err := s.BeginMediaUpload("video/webm")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AppendMedia(u.ID, 0, []byte("pending"), false); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(dir, "media"), 0700); err != nil {
		t.Fatal(err)
	}
	temp := filepath.Join(dir, "media", ".upload-abandoned")
	if err = os.WriteFile(temp, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reopened.AppendMedia(u.ID, 7, nil, true); err == nil {
		t.Fatal("reused interrupted session")
	}
	for _, p := range []string{temp, filepath.Join(dir, ".media-"+u.ID)} {
		if _, err = os.Stat(p); !os.IsNotExist(err) {
			t.Fatal("abandoned upload remains", p, err)
		}
	}
}
