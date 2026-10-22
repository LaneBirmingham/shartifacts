package store

import (
	"errors"
	"strings"
	"testing"
)

func TestArtifactStorage(t *testing.T) {
	dir := t.TempDir()
	s, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	a, _ := s.Save("", Input{Title: "Poll", Kind: "html", Content: "x"})
	if e = s.StorageSet("missing", "k", "v", false); !errors.Is(e, ErrNotFound) {
		t.Fatalf("storage for a missing artifact: %v", e)
	}
	for _, bad := range []string{"", "a b", "a/b", `a"b`, "a'b", strings.Repeat("k", 201)} {
		if e = s.StorageSet(a.ID, bad, "v", false); e == nil {
			t.Fatalf("bad key %q accepted", bad)
		}
	}
	if e = s.StorageSet(a.ID, "votes:alice", "1", false); e != nil {
		t.Fatal(e)
	}
	if e = s.StorageSet(a.ID, "votes:bob", "2", true); e != nil {
		t.Fatal(e)
	}
	// Scopes are separate.
	if _, ok, _ := s.StorageGet(a.ID, "votes:bob", false); ok {
		t.Fatal("shared key visible in personal scope")
	}
	if e = s.StorageSet(a.ID, "big", strings.Repeat("x", MaxStorageValue+1), false); e == nil {
		t.Fatal("oversized value accepted")
	}
	for i := 0; i < 3; i++ {
		if e = s.StorageSet(a.ID, "fill"+string(rune('a'+i)), strings.Repeat("x", MaxStorageValue), true); e != nil {
			t.Fatal(e)
		}
	}
	if e = s.StorageSet(a.ID, "fulld", strings.Repeat("x", MaxStorageValue), true); !errors.Is(e, ErrStorageFull) {
		t.Fatalf("over quota: %v", e)
	}
	// Replacing a value counts only the difference.
	if e = s.StorageSet(a.ID, "filla", strings.Repeat("y", MaxStorageValue), true); e != nil {
		t.Fatal(e)
	}
	reopened, _ := Open(dir)
	if v, ok, e := reopened.StorageGet(a.ID, "votes:alice", false); !ok || v != "1" || e != nil {
		t.Fatal("storage not durable", v, ok, e)
	}
	keys, _ := reopened.StorageList(a.ID, "votes:", true)
	if strings.Join(keys, ",") != "votes:bob" {
		t.Fatal(keys)
	}
	if deleted, _ := reopened.StorageDelete(a.ID, "votes:alice", false); !deleted {
		t.Fatal("delete reported nothing deleted")
	}
	if deleted, _ := reopened.StorageDelete(a.ID, "votes:alice", false); deleted {
		t.Fatal("second delete reported a deletion")
	}
	if e = reopened.Delete(a.ID, 1); e != nil {
		t.Fatal(e)
	}
	b, _ := reopened.Save("", Input{Title: "Other", Kind: "html", Content: "x"})
	if keys, _ := reopened.StorageList(b.ID, "", true); len(keys) != 0 {
		t.Fatal("storage leaked across artifacts")
	}
	if _, e = reopened.StorageList(a.ID, "", true); !errors.Is(e, ErrNotFound) {
		t.Fatal("deleted artifact kept storage")
	}
}
