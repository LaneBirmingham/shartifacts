package store

import (
	"errors"
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
	if e = reopened.Delete(a.ID, 1); e != nil {
		t.Fatal(e)
	}
	if again, _ := Open(dir); len(again.comments) != 0 {
		t.Fatal("comments outlived their artifact")
	}
}
