package server

import (
	"hash/fnv"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LaneBirmingham/shartifacts/internal/search"
	"github.com/LaneBirmingham/shartifacts/internal/store"
)

// searchCache keeps each artifact's indexed text until it changes, and the index over
// them until any does, so searching as someone types stays cheap.
type searchCache struct {
	mu    sync.Mutex
	docs  map[string]cachedDoc
	sig   uint64
	index *search.Index
}
type cachedDoc struct {
	key uint64
	doc *search.Doc
}

// searchState is the current index with every artifact's summary and search metadata.
func (s *Server) searchState() (*search.Index, map[string]store.Summary, map[string]search.Meta) {
	sources := s.Store.SearchSources()
	slices.SortFunc(sources, func(a, b store.SearchSource) int { return strings.Compare(a.ID, b.ID) })
	summaries := make(map[string]store.Summary, len(sources))
	meta := make(map[string]search.Meta, len(sources))
	c := &s.search
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.docs == nil {
		c.docs = map[string]cachedDoc{}
	}
	all := fnv.New64a()
	docs := make([]*search.Doc, 0, len(sources))
	next := make(map[string]cachedDoc, len(sources))
	for _, src := range sources {
		summaries[src.ID] = src.Summary
		meta[src.ID] = search.Meta{Pinned: src.Pinned, Created: src.CreatedAt, Updated: src.UpdatedAt, Comments: src.Summary.Comments, OpenComments: src.OpenComments}
		key := textKey(src)
		cached, ok := c.docs[src.ID]
		if !ok || cached.key != key {
			cached = cachedDoc{key, search.NewDoc(search.Source{ID: src.ID, Kind: src.Kind, Title: src.Title, Description: src.Description,
				Tags: src.Tags, Content: src.Content, Files: src.Files, Notes: src.Notes, Comments: src.CommentText})}
		}
		next[src.ID] = cached
		docs = append(docs, cached.doc)
		all.Write([]byte(src.ID + "\x00" + strconv.FormatUint(key, 16) + "\x00"))
	}
	c.docs = next
	if sig := all.Sum64(); c.index == nil || sig != c.sig {
		c.index, c.sig = search.Build(docs), sig
	}
	return c.index, summaries, meta
}

// WarmSearch indexes the library ahead of the first search.
func (s *Server) WarmSearch() { s.searchState() }

// textKey changes whenever an artifact's searchable text does.
func textKey(src store.SearchSource) uint64 {
	h := fnv.New64a()
	for _, part := range append([]string{strconv.Itoa(src.Version), src.Description, strings.Join(src.Tags, ","), strconv.Itoa(len(src.Notes))}, src.CommentText...) {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return h.Sum64()
}

// searchResult is a library summary with why and where it matched.
type searchResult struct {
	store.Summary
	search.Hit
}

type searchResponse struct {
	Results    []searchResult `json:"results"`
	Total      int            `json:"total"`
	Suggestion string         `json:"suggestion,omitempty"`
	Query      search.Query   `json:"query"`
}

// runSearch parses the search box text plus any separate filters and ranks the library.
func (s *Server) runSearch(text string, filters map[string][]string, sortBy string, limit int) searchResponse {
	now := time.Now()
	q := search.Parse(text, now)
	for key, values := range filters {
		for _, v := range values {
			if v != "" {
				q.Filter(key, v, now)
			}
		}
	}
	index, summaries, meta := s.searchState()
	res := index.Search(q, meta, search.Options{Sort: sortBy, Limit: limit, Now: now})
	out := searchResponse{Results: []searchResult{}, Total: res.Total, Suggestion: search.Correct(text, res.Corrections), Query: q}
	for _, h := range res.Hits {
		if sum, ok := summaries[h.ID]; ok {
			out.Results = append(out.Results, searchResult{sum, h})
		}
	}
	return out
}

type relatedResult struct {
	store.Summary
	search.Relation
}

func (s *Server) searchRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/search", func(w http.ResponseWriter, r *http.Request) {
		v := r.URL.Query()
		limit, _ := strconv.Atoi(v.Get("limit"))
		filters := map[string][]string{}
		for _, key := range []string{"kind", "tag", "has", "updated", "created", "is"} {
			filters[key] = v[key]
		}
		writeJSON(w, 200, s.runSearch(v.Get("q"), filters, v.Get("sort"), min(max(limit, 0), 200)))
	})
	mux.HandleFunc("GET /api/artifacts/{id}/related", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		index, summaries, _ := s.searchState()
		if _, ok := summaries[id]; !ok {
			respond(w, 404, nil, store.ErrNotFound)
			return
		}
		related := []relatedResult{}
		for _, rel := range index.Related(id, 5) {
			if sum, ok := summaries[rel.ID]; ok {
				related = append(related, relatedResult{sum, rel})
			}
		}
		tags := index.SuggestTags(id, 6)
		if tags == nil {
			tags = []string{}
		}
		writeJSON(w, 200, map[string]any{"related": related, "suggested_tags": tags})
	})
}
