package store

// SearchSource is an artifact's library summary with the text search reads: its latest
// source and files, every revision's note and its comments.
type SearchSource struct {
	Summary
	Content string
	Files   map[string]string
	Notes   []string
	// CommentText is each comment as "author: body".
	CommentText []string
}

// SearchSources returns every artifact's SearchSource, sharing (not copying) the text.
func (s *Store) SearchSources() []SearchSource {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SearchSource, 0, len(s.items))
	for _, a := range s.items {
		r := a.Revisions[len(a.Revisions)-1]
		src := SearchSource{Summary: s.summary(a), Content: r.Content, Files: r.Files}
		for _, rev := range a.Revisions {
			if rev.Note != "" {
				src.Notes = append(src.Notes, rev.Note)
			}
		}
		for _, c := range s.comments[a.ID] {
			src.CommentText = append(src.CommentText, c.Author+": "+c.Body)
		}
		out = append(out, src)
	}
	return out
}
