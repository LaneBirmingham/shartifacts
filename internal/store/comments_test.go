package store

import (
	"errors"
	"strings"
	"testing"
)

func TestComments(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	a, _ := s.Save("", Input{Title: "Page", Kind: "html", Content: "x"})
	if _, e := s.AddComment("nope", CommentInput{Body: "hi"}); !errors.Is(e, ErrNotFound) {
		t.Fatal(e)
	}
	if _, e := s.AddComment(a.ID, CommentInput{Body: "  "}); e == nil {
		t.Fatal("empty comment accepted")
	}
	root, e := s.AddComment(a.ID, CommentInput{Body: "Make the button bigger", Quote: "<button>"})
	if e != nil || root.Author != "you" || root.Version != 1 {
		t.Fatal(e, root)
	}
	reply, e := s.AddComment(a.ID, CommentInput{Parent: root.ID, Author: "agent", Body: "Done in v2"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.AddComment(a.ID, CommentInput{Parent: reply.ID, Body: "nested"}); e == nil {
		t.Fatal("reply to a reply accepted")
	}
	if _, e = s.ResolveComment(a.ID, reply.ID, true); e == nil {
		t.Fatal("resolved a reply")
	}
	if s.List(ExcludeArchived)[0].OpenComments != 1 {
		t.Fatal("open thread not counted")
	}
	if _, e = s.ResolveComment(a.ID, root.ID, true); e != nil {
		t.Fatal(e)
	}
	reopened, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	list, _ := reopened.Comments(a.ID)
	if len(list) != 2 || !list[0].Resolved || list[1].Parent != root.ID || reopened.List(ExcludeArchived)[0].OpenComments != 0 {
		t.Fatal(list)
	}
	// A comment made on the preview keeps what it pointed at and the version it was made on.
	b, e := reopened.Save(a.ID, Input{Title: "Page", Kind: "html", Content: "y", BaseVersion: 1})
	if e != nil {
		t.Fatal(e)
	}
	pointed, e := reopened.AddComment(b.ID, CommentInput{Body: "Bolder", Quote: "Totals", Anchor: "main > h2", Element: `<h2 class="title">`, Version: 1})
	if e != nil || pointed.Element != `<h2 class="title">` || pointed.Anchor != "main > h2" || pointed.Version != 1 {
		t.Fatal(e, pointed)
	}
	if latest, _ := reopened.AddComment(b.ID, CommentInput{Body: "Latest"}); latest.Version != 2 {
		t.Fatal("version did not default to the latest", latest)
	}
	for _, bad := range []CommentInput{{Body: "x", Version: 3}, {Body: "x", Version: -1}, {Body: "x", Element: "<" + strings.Repeat("a", 2000) + ">"}} {
		if _, e = reopened.AddComment(b.ID, bad); e == nil {
			t.Fatal("accepted", bad)
		}
	}
	if e = reopened.Delete(a.ID, 2); e != nil {
		t.Fatal(e)
	}
	if again, _ := Open(dir); len(again.comments) != 0 {
		t.Fatal("comments outlived their artifact")
	}
}
