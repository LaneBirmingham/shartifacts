package search

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// corpus writes n markdown artifacts that share a vocabulary the way a real library does:
// each draws its words from the same few thousand terms, some of them inflected.
func corpus(n, words int) []Source {
	syllables := strings.Fields("ba ke mo ri tu la ne so vi do pa re mi to ku fa")
	vocab := make([]string, 3000)
	for i := range vocab {
		vocab[i] = syllables[i%16] + syllables[i/16%16] + syllables[i/256%16] + syllables[i/7%16]
	}
	endings := []string{"", "", "", "s", "ing"}
	state := uint32(1)
	next := func() int { state = state*1664525 + 1013904223; return int(state >> 8) }
	out := make([]Source, n)
	for i := range out {
		var b strings.Builder
		for j := 0; j < words; j++ {
			b.WriteString(vocab[next()%len(vocab)] + endings[next()%len(endings)] + " ")
		}
		out[i] = Source{ID: fmt.Sprintf("doc%d", i), Kind: "markdown", Title: fmt.Sprintf("Document %d", i), Content: b.String()}
	}
	return out
}

// TestIndexedDocsStayNearTheirTextSize keeps the in-memory index of a library at a small
// multiple of its text. The index of a real library once held about nine times its text,
// most of it a map of spellings kept for every term of every artifact, which put an idle
// server of a few hundred pages at over 200 MB.
func TestIndexedDocsStayNearTheirTextSize(t *testing.T) {
	sources := corpus(150, 3000)
	text := 0
	for _, s := range sources {
		text += len(s.Content)
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	docs := make([]*Doc, len(sources))
	for i, s := range sources {
		docs[i] = NewDoc(s)
	}
	ix := Build(docs)
	runtime.GC()
	runtime.ReadMemStats(&after)
	held := int(after.HeapAlloc) - int(before.HeapAlloc)
	t.Logf("%d bytes of text held as %d bytes (%.1fx)", text, held, float64(held)/float64(text))
	if limit := 16 * text; held > limit {
		t.Errorf("indexing %d bytes of text holds %d bytes, over %d", text, held, limit)
	}
	runtime.KeepAlive(ix)
	runtime.KeepAlive(docs)
}
