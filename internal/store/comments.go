package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A Comment is feedback on an artifact. A thread is a root comment (Parent empty) and its
// replies; only roots are resolved. Comments live beside the artifact, outside its
// revisions, and are deleted with it.
type Comment struct {
	ID     string `json:"id"`
	Parent string `json:"parent,omitempty"`
	Author string `json:"author"`
	Body   string `json:"body"`
	Quote  string `json:"quote,omitempty"`
	// Anchor is where a page placed the comment (a CSS path), and ToClaude marks one the
	// person sent to their agent ("Send to Claude").
	Anchor    string    `json:"anchor,omitempty"`
	ToClaude  bool      `json:"to_claude,omitempty"`
	Version   int       `json:"version"`
	Resolved  bool      `json:"resolved,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type CommentInput struct {
	Parent   string `json:"parent,omitempty"`
	Author   string `json:"author,omitempty"`
	Body     string `json:"body"`
	Quote    string `json:"quote,omitempty"`
	Anchor   string `json:"anchor,omitempty"`
	ToClaude bool   `json:"to_claude,omitempty"`
}

const maxComments = 1000

func (s *Store) commentsPath(id string) string {
	return filepath.Join(s.dir, "comments", id+".json")
}

func (s *Store) loadComments() error {
	files, err := filepath.Glob(filepath.Join(s.dir, "comments", "*.json"))
	if err != nil {
		return err
	}
	s.comments = map[string][]Comment{}
	for _, f := range files {
		id := strings.TrimSuffix(filepath.Base(f), ".json")
		if _, ok := s.items[id]; !ok {
			continue // left behind by an interrupted delete
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		var list []Comment
		if err = json.Unmarshal(b, &list); err != nil {
			return fmt.Errorf("read %s: %w", f, err)
		}
		s.comments[id] = list
	}
	return nil
}

// Comments returns an artifact's comments, oldest first.
func (s *Store) Comments(id string) ([]Comment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[id]; !ok {
		return nil, ErrNotFound
	}
	return append([]Comment{}, s.comments[id]...), nil
}

func (s *Store) AddComment(id string, in CommentInput) (Comment, error) {
	in.Body = strings.TrimSpace(in.Body)
	if in.Body == "" || len(in.Body) > 10000 {
		return Comment{}, errors.New("comment must contain 1–10000 bytes")
	}
	if len(in.Quote) > 2000 || len(in.Anchor) > 2000 {
		return Comment{}, errors.New("quote and anchor are at most 2000 bytes")
	}
	author := strings.TrimSpace(in.Author)
	if author == "" {
		author = "you"
	}
	if len(author) > 60 {
		return Comment{}, errors.New("author exceeds 60 bytes")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.items[id]
	if !ok {
		return Comment{}, ErrNotFound
	}
	list := s.comments[id]
	if len(list) >= maxComments {
		return Comment{}, fmt.Errorf("an artifact holds at most %d comments", maxComments)
	}
	if in.Parent != "" {
		root := -1
		for i, c := range list {
			if c.ID == in.Parent {
				root = i
			}
		}
		if root < 0 || list[root].Parent != "" {
			return Comment{}, errors.New("parent must be the ID of a thread's first comment")
		}
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return Comment{}, err
	}
	c := Comment{ID: hex.EncodeToString(b), Parent: in.Parent, Author: author, Body: in.Body, Quote: in.Quote, Anchor: in.Anchor, ToClaude: in.ToClaude, Version: len(a.Revisions), CreatedAt: time.Now().UTC()}
	if err := s.writeComments(id, append(append([]Comment{}, list...), c)); err != nil {
		return Comment{}, err
	}
	go s.changed(id, "comments", c.ID)
	return c, nil
}

// ResolveComment marks a thread resolved or reopens it.
func (s *Store) ResolveComment(id, commentID string, resolved bool) (Comment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[id]; !ok {
		return Comment{}, ErrNotFound
	}
	list := append([]Comment{}, s.comments[id]...)
	for i, c := range list {
		if c.ID == commentID {
			if c.Parent != "" {
				return Comment{}, errors.New("resolve the thread's first comment")
			}
			list[i].Resolved = resolved
			if err := s.writeComments(id, list); err != nil {
				return Comment{}, err
			}
			go s.changed(id, "comments", commentID)
			return list[i], nil
		}
	}
	return Comment{}, errors.New("comment not found")
}

// DeleteThread removes a thread and its replies; false when there is no such thread.
func (s *Store) DeleteThread(id, threadID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[id]; !ok {
		return false, ErrNotFound
	}
	keep, found := []Comment{}, false
	for _, c := range s.comments[id] {
		if c.ID == threadID && c.Parent == "" {
			found = true
		} else if c.Parent != threadID {
			keep = append(keep, c)
		}
	}
	if !found {
		return false, nil
	}
	if err := s.writeComments(id, keep); err != nil {
		return false, err
	}
	go s.changed(id, "comments", threadID)
	return true, nil
}

// writeComments persists an artifact's comments. s.mu is held.
func (s *Store) writeComments(id string, list []Comment) error {
	dir := filepath.Join(s.dir, "comments")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err = writeAtomic(dir, s.commentsPath(id), b); err != nil {
		return err
	}
	if s.comments == nil {
		s.comments = map[string][]Comment{}
	}
	s.comments[id] = list
	return nil
}

// openThreads counts unresolved threads. s.mu is held.
func (s *Store) openThreads(id string) int {
	n := 0
	for _, c := range s.comments[id] {
		if c.Parent == "" && !c.Resolved {
			n++
		}
	}
	return n
}
