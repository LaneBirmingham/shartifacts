package search

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Source is what an artifact contributes to search: its latest revision and the
// unversioned metadata and comments around it.
type Source struct {
	ID, Kind, Title, Description string
	Tags                         []string
	Content                      string
	Files                        map[string]string
	Notes                        []string
	Comments                     []string
}

// Meta is library state that changes without the text changing, read at search time.
type Meta struct {
	Pinned, Archived       bool
	Created, Updated       time.Time
	Comments, OpenComments int
}

type field int

const (
	fTitle field = iota
	fTags
	fDescription
	fKind
	fHeadings
	fBody
	fNotes
	fComments
	fCode
	nFields
)

var (
	fieldNames  = [nFields]string{"title", "tags", "description", "type", "content", "content", "notes", "comments", "source"}
	fieldWeight = [nFields]float64{8, 6, 4, 2, 3, 1, 0.8, 1, 0.3}
	// relatedWeight shapes the vectors that relate artifacts: what they are about, not
	// the boilerplate their source shares.
	relatedWeight = [nFields]float64{3, 3, 2, 0, 1.5, 1, 0.5, 0.5, 0.2}
	// snippetFields are those whose text reads well as a result's excerpt.
	snippetFields = []field{fDescription, fHeadings, fBody, fComments, fNotes, fCode}
)

const (
	k1      = 1.2
	b       = 0.75
	maxText = 1 << 20
)

// Doc is one artifact's indexed text. Building it is the costly part of indexing, so
// callers keep it until the artifact's text changes.
type Doc struct {
	ID, Kind string
	Tags     []string
	// Traits are what it contains: video, audio, image, chart, diagram, table, form, 3d,
	// math, data.
	Traits []string
	files  bool
	text   [nFields]string
	tf     [nFields]map[string]int32
	length [nFields]int
	// forms counts how each indexed term was written, for corrections and labels.
	forms  map[string]map[string]int
	folded string
}

func NewDoc(src Source) *Doc {
	var t text
	t.add(src.Kind, src.Content)
	sources := []string{src.Content}
	names := make([]string, 0, len(src.Files))
	for name := range src.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		t.add(fileRole(name), src.Files[name])
		sources = append(sources, src.Files[name])
	}
	d := &Doc{ID: src.ID, Kind: src.Kind, Tags: src.Tags, Traits: traits(src.Kind, sources), files: len(names) > 0}
	d.text = [nFields]string{
		fTitle:       src.Title,
		fTags:        strings.Join(src.Tags, " "),
		fDescription: src.Description,
		fKind:        strings.TrimSpace(kindWords[src.Kind] + " " + strings.Join(d.Traits, " ")),
		fHeadings:    strings.Join(t.headings, "\n"),
		fBody:        strings.Join(t.body, "\n"),
		fNotes:       strings.Join(src.Notes, "\n"),
		fComments:    strings.Join(src.Comments, "\n"),
		fCode:        strings.Join(append(t.code, names...), "\n"),
	}
	votes := map[string]map[string]int{}
	var folded strings.Builder
	for f := range d.text {
		if len(d.text[f]) > maxText {
			d.text[f] = strings.ToValidUTF8(d.text[f][:maxText], "")
		}
		tf, n := map[string]int32{}, 0
		words(d.text[f], func(w string, _, _ int) {
			nw := normal(w)
			if field(f) != fKind {
				folded.WriteString(nw)
				folded.WriteByte(' ')
			}
			t := term(w)
			if t == "" || field(f) == fCode && codeWords[t] {
				return
			}
			tf[t]++
			n++
			if field(f) != fCode && field(f) != fKind {
				if votes[t] == nil {
					votes[t] = map[string]int{}
				}
				votes[t][nw]++
			}
		})
		if field(f) != fKind {
			folded.WriteByte('\n')
		}
		d.tf[f], d.length[f] = tf, n
	}
	d.folded = folded.String()
	d.forms = votes
	return d
}

// commonest picks the most used form, preferring the shorter on a tie.
func commonest(forms map[string]int) string {
	best, n := "", 0
	for w, c := range forms {
		if c > n || c == n && (len(w) < len(best) || len(w) == len(best) && w < best) {
			best, n = w, c
		}
	}
	return best
}

// Index ranks a set of Docs. It is immutable; build a new one when they change.
type Index struct {
	docs    []*Doc
	byID    map[string]int
	df      map[string]int
	avg     [nFields]float64
	vocab   []string
	surface map[string]string
	forms   map[string][]string // every written form of a stem
	tags    map[string]string   // stem of a one-word tag → the tag
	vec     []map[string]float64
}

func Build(docs []*Doc) *Index {
	ix := &Index{docs: docs, byID: map[string]int{}, df: map[string]int{}, surface: map[string]string{}, forms: map[string][]string{}, tags: map[string]string{}}
	votes := map[string]map[string]int{}
	for i, d := range docs {
		ix.byID[d.ID] = i
		seen := map[string]bool{}
		for f := range d.tf {
			ix.avg[f] += float64(d.length[f])
			for t := range d.tf[f] {
				if !seen[t] {
					seen[t] = true
					ix.df[t]++
				}
			}
		}
		for t, forms := range d.forms {
			if votes[t] == nil {
				votes[t] = map[string]int{}
			}
			for w, n := range forms {
				votes[t][w] += n
			}
		}
		for _, tag := range d.Tags {
			if t := term(tag); t != "" && !strings.ContainsAny(tag, "- ") {
				ix.tags[t] = tag
			}
		}
	}
	for f := range ix.avg {
		ix.avg[f] = max(ix.avg[f]/float64(max(1, len(docs))), 1)
	}
	ix.vocab = make([]string, 0, len(ix.df))
	for t := range ix.df {
		ix.vocab = append(ix.vocab, t)
		if forms := votes[t]; forms != nil {
			ix.surface[t] = commonest(forms)
			for w := range forms {
				ix.forms[t] = append(ix.forms[t], w)
			}
			sort.Strings(ix.forms[t])
		} else {
			ix.surface[t] = t
		}
	}
	sort.Strings(ix.vocab)
	ix.vec = make([]map[string]float64, len(docs))
	for i, d := range docs {
		ix.vec[i] = ix.vector(d)
	}
	return ix
}

func (ix *Index) idf(t string) float64 {
	n, df := float64(len(ix.docs)), float64(ix.df[t])
	return math.Log(1 + (n-df+0.5)/(df+0.5))
}

// vector is d's unit tf-idf vector over its most telling 150 terms.
func (ix *Index) vector(d *Doc) map[string]float64 {
	v := map[string]float64{}
	for f := range d.tf {
		if relatedWeight[f] == 0 {
			continue
		}
		for t, n := range d.tf[f] {
			if !numeric(t) {
				v[t] += relatedWeight[f] * (1 + math.Log(float64(n)))
			}
		}
	}
	type kv struct {
		t string
		w float64
	}
	all := make([]kv, 0, len(v))
	for t, w := range v {
		all = append(all, kv{t, w * ix.idf(t)})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].w > all[j].w || all[i].w == all[j].w && all[i].t < all[j].t })
	if len(all) > 150 {
		all = all[:150]
	}
	var norm float64
	for _, e := range all {
		norm += e.w * e.w
	}
	out := make(map[string]float64, len(all))
	for _, e := range all {
		if e.w > 0 {
			out[e.t] = e.w / math.Sqrt(norm)
		}
	}
	return out
}

func commonPrefix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

func numeric(t string) bool {
	for _, r := range t {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// expand finds the indexed terms a query word stands for, with how strongly each counts:
// the word itself, words it starts (the last word is still being typed), its synonyms,
// and, when it matches nothing as written, words a typo or two away. correction is the
// likeliest intended word in that last case.
func (ix *Index) expand(word string, last bool) (exps map[string]float64, correction string) {
	exps = map[string]float64{}
	add := func(t string, w float64) {
		if exps[t] < w {
			exps[t] = w
		}
	}
	w := normal(word)
	s := term(word)
	if s == "" {
		return exps, ""
	}
	direct := ix.df[s] > 0
	if direct {
		add(s, 1)
	}
	if n := len([]rune(w)); n >= 3 || last && n >= 2 {
		for i, seen := sort.SearchStrings(ix.vocab, w), 0; i < len(ix.vocab) && seen < 40 && strings.HasPrefix(ix.vocab[i], w); i, seen = i+1, seen+1 {
			add(ix.vocab[i], 0.8)
			direct = true
		}
	}
	for _, syn := range synonyms[s] {
		if ix.df[syn] > 0 {
			add(syn, 0.6)
		}
	}
	if n := len([]rune(w)); n >= 4 && (!direct || ix.df[s] <= 1) {
		limit := 1
		if n >= 7 {
			limit = 2
		}
		best, bestD, bestLead, bestWritten := "", limit+1, 0, 0
		for _, t := range ix.vocab {
			if t == s || abs(len(t)-len(s)) > limit {
				continue
			}
			// Close once endings are stripped, and still close as people write it: lons is
			// loans, not lane.
			d := distance(s, t, limit)
			if d > limit {
				continue
			}
			written := distance(w, ix.surface[t], limit+1)
			if written > limit+1 {
				continue
			}
			add(t, []float64{0, 0.7, 0.45}[d])
			// Ties go to the word that starts the same way, then reads closer, then is commoner.
			lead := commonPrefix(s, t)
			if d < bestD || d == bestD && (lead > bestLead || lead == bestLead && (written < bestWritten || written == bestWritten && ix.df[t] > ix.df[best])) {
				best, bestD, bestLead, bestWritten = t, d, lead, written
			}
		}
		if best != "" && !direct {
			// Offer the written form nearest what was typed: lons is loans, not loan.
			correction = ix.surface[best]
			near := distance(w, correction, 9)
			for _, form := range ix.forms[best] {
				if d := distance(w, form, 9); d < near {
					correction, near = form, d
				}
			}
			for _, syn := range synonyms[best] {
				if ix.df[syn] > 0 {
					add(syn, 0.4)
				}
			}
		}
	}
	return exps, correction
}

// Segment is a run of a title or excerpt, Match when it is a word the query found.
type Segment struct {
	Text  string `json:"text"`
	Match bool   `json:"match,omitempty"`
}

type Hit struct {
	ID    string  `json:"-"`
	Score float64 `json:"score"`
	// Matched names where the query words were found: title, tags, description, type,
	// content, notes, comments, source.
	Matched       []string  `json:"matched,omitempty"`
	TitleSegments []Segment `json:"title_segments,omitempty"`
	Snippet       []Segment `json:"snippet,omitempty"`
	Traits        []string  `json:"traits,omitempty"`
}

type Results struct {
	Hits  []Hit
	Total int
	// Corrections maps query words that matched nothing as typed to the likeliest word.
	Corrections map[string]string
}

type Options struct {
	// Sort is relevance, updated, oldest, created or title; empty is relevance for a query with
	// words and updated (pinned first) otherwise.
	Sort  string
	Limit int
	Now   time.Time
}

// Search ranks the documents matching q with BM25 over weighted fields.
func (ix *Index) Search(q Query, meta map[string]Meta, opt Options) Results {
	type qword struct {
		exps map[string]float64
	}
	var qs []qword
	res := Results{Corrections: map[string]string{}}
	for i, w := range q.Words {
		exps, fix := ix.expand(w, i == len(q.Words)-1)
		if fix != "" && !strings.EqualFold(fix, w) {
			res.Corrections[w] = fix
		}
		qs = append(qs, qword{exps})
	}
	var exclude []string
	for _, w := range q.Exclude {
		if t := term(w); t != "" {
			exclude = append(exclude, t)
		}
	}
	var phrases []string
	for _, p := range q.Phrases {
		if f := squash(p); f != "" {
			phrases = append(phrases, f)
		}
	}
	if opt.Now.IsZero() {
		opt.Now = time.Now()
	}
	sortBy := opt.Sort
	if sortBy == "" {
		sortBy = "updated"
		if len(qs) > 0 {
			sortBy = "relevance"
		}
	}
	for _, d := range ix.docs {
		m := meta[d.ID]
		if !q.passes(d, m) || ix.excluded(d, exclude) || !containsPhrases(d.folded, phrases) {
			continue
		}
		var total float64
		var contrib [nFields]float64
		found := map[string]bool{}
		matched := 0
		for _, w := range qs {
			var best float64
			var bestPer [nFields]float64
			for t, weight := range w.exps {
				idf := ix.idf(t)
				var sum float64
				var per [nFields]float64
				for f := range d.tf {
					n := float64(d.tf[f][t])
					if n == 0 {
						continue
					}
					norm := 1 - b + b*float64(d.length[f])/ix.avg[f]
					per[f] = weight * idf * fieldWeight[f] * n * (k1 + 1) / (n + k1*norm)
					sum += per[f]
				}
				if sum > 0 {
					found[t] = true
				}
				if sum > best {
					best, bestPer = sum, per
				}
			}
			if best > 0 {
				matched++
				total += best
				for f := range contrib {
					contrib[f] += bestPer[f]
				}
			}
		}
		if len(qs) > 0 {
			if matched == 0 || len(qs) >= 3 && matched*2 < len(qs) {
				continue
			}
			coverage := float64(matched) / float64(len(qs))
			total *= coverage * coverage
			// A gentle lean towards what was touched lately.
			age := opt.Now.Sub(m.Updated).Hours() / 24
			total *= 1 + 0.15*math.Exp(-max(age, 0)/30)
		}
		hit := Hit{ID: d.ID, Score: math.Round(total*1000) / 1000, Traits: d.Traits}
		seen := map[string]bool{}
		for f, c := range contrib {
			if c > 0 && !seen[fieldNames[f]] {
				seen[fieldNames[f]] = true
				hit.Matched = append(hit.Matched, fieldNames[f])
			}
		}
		if len(found) > 0 {
			match := func(t string) bool { return found[t] }
			hit.TitleSegments = highlight(d.text[fTitle], match, 0)
			best := field(-1)
			for _, f := range snippetFields {
				if contrib[f] > 0 && (best < 0 || contrib[f] > contrib[best]) {
					best = f
				}
			}
			if best >= 0 && best != fDescription {
				hit.Snippet = highlight(d.text[best], match, 160)
			} else if best == fDescription {
				hit.Snippet = highlight(d.text[best], match, 0)
			}
		}
		res.Hits = append(res.Hits, hit)
	}
	res.Total = len(res.Hits)
	ix.sortHits(res.Hits, meta, sortBy, len(qs) > 0)
	if opt.Limit <= 0 {
		opt.Limit = 50
	}
	if len(res.Hits) > opt.Limit {
		res.Hits = res.Hits[:opt.Limit]
	}
	return res
}

func (ix *Index) sortHits(hits []Hit, meta map[string]Meta, by string, ranked bool) {
	title := func(h Hit) string { return normal(ix.docs[ix.byID[h.ID]].text[fTitle]) }
	sort.SliceStable(hits, func(i, j int) bool {
		a, c := meta[hits[i].ID], meta[hits[j].ID]
		if !ranked && by == "updated" && a.Pinned != c.Pinned {
			return a.Pinned
		}
		switch by {
		case "relevance":
			if hits[i].Score != hits[j].Score {
				return hits[i].Score > hits[j].Score
			}
		case "created":
			if !a.Created.Equal(c.Created) {
				return a.Created.After(c.Created)
			}
		case "oldest":
			// The least recently updated first: what a tidy-up looks at.
			if !a.Updated.Equal(c.Updated) {
				return a.Updated.Before(c.Updated)
			}
		case "title":
			if ti, tj := title(hits[i]), title(hits[j]); ti != tj {
				return ti < tj
			}
		}
		if !a.Updated.Equal(c.Updated) {
			return a.Updated.After(c.Updated)
		}
		return hits[i].ID < hits[j].ID
	})
}

var kindAlias = map[string]string{
	"page": "html", "web": "html", "app": "react", "jsx": "react", "md": "markdown", "doc": "markdown",
	"document": "markdown", "deck": "slides", "slide": "slides", "presentation": "slides",
	"diagram": "mermaid", "image": "svg", "snippet": "code",
}

var hasAlias = map[string]string{
	"comment": "comments", "thread": "comments", "open": "open", "unresolved": "open",
	"file": "files", "tag": "tags", "picture": "image", "photo": "image", "img": "image",
	"movie": "video", "graph": "chart", "plot": "chart", "music": "audio", "sound": "audio",
	"input": "form", "flowchart": "diagram", "equation": "math", "three": "3d",
}

func (q Query) passes(d *Doc, m Meta) bool {
	if len(q.Kinds) > 0 {
		ok := false
		for _, k := range q.Kinds {
			if a, aliased := kindAlias[k]; aliased {
				k = a
			}
			ok = ok || k == d.Kind
		}
		if !ok {
			return false
		}
	}
	for _, t := range q.Tags {
		if !contains(d.Tags, t) {
			return false
		}
	}
	if q.Pinned && !m.Pinned {
		return false
	}
	switch q.Archived {
	case "only":
		if !m.Archived {
			return false
		}
	case "":
		if m.Archived {
			return false
		}
	}
	for _, h := range q.Has {
		if a, ok := hasAlias[h]; ok {
			h = a
		}
		switch h {
		case "comments":
			if m.Comments == 0 {
				return false
			}
		case "open":
			if m.OpenComments == 0 {
				return false
			}
		case "files":
			if !d.files {
				return false
			}
		case "tags":
			if len(d.Tags) == 0 {
				return false
			}
		default:
			if !contains(d.Traits, h) {
				return false
			}
		}
	}
	return within(m.Updated, q.UpdatedAfter, q.UpdatedBefore) && within(m.Created, q.CreatedAfter, q.CreatedBefore)
}

func within(t, after, before time.Time) bool {
	return (after.IsZero() || !t.Before(after)) && (before.IsZero() || t.Before(before))
}

func (ix *Index) excluded(d *Doc, terms []string) bool {
	for _, t := range terms {
		for f := range d.tf {
			if d.tf[f][t] > 0 {
				return true
			}
		}
	}
	return false
}

// squash is text as phrase matching compares it: normalised words between spaces.
func squash(s string) string {
	var out strings.Builder
	words(s, func(w string, _, _ int) {
		out.WriteString(normal(w))
		out.WriteByte(' ')
	})
	return strings.TrimSpace(out.String())
}

func containsPhrases(folded string, phrases []string) bool {
	for _, p := range phrases {
		i := strings.Index(folded, p)
		for i >= 0 {
			end := i + len(p)
			if (i == 0 || folded[i-1] == ' ' || folded[i-1] == '\n') && (end == len(folded) || folded[end] == ' ' || folded[end] == '\n') {
				break
			}
			next := strings.Index(folded[i+1:], p)
			if next < 0 {
				i = -1
			} else {
				i += 1 + next
			}
		}
		if i < 0 {
			return false
		}
	}
	return true
}

var whitespace = regexp.MustCompile(`\s+`)

// highlight splits text into segments marking the words match accepts. A positive width
// trims it to an excerpt of about that many bytes around the first match.
func highlight(text string, match func(string) bool, width int) []Segment {
	type span struct{ start, end int }
	var spans []span
	words(text, func(w string, start, end int) {
		if t := term(w); t != "" && match(t) {
			spans = append(spans, span{start, end})
		}
	})
	from, to := 0, len(text)
	if width > 0 {
		if len(spans) == 0 {
			return nil
		}
		// Little lead-in, so the match shows within a two-line excerpt.
		from = max(0, spans[0].start-width/6)
		if from > 0 {
			if sp := strings.IndexAny(text[from:spans[0].start], " \n\t"); sp >= 0 {
				from += sp + 1
			} else {
				from = spans[0].start
			}
		}
		to = min(len(text), from+width)
		if to < len(text) {
			if sp := strings.LastIndexAny(text[spans[0].end:to], " \n\t"); sp >= 0 {
				to = spans[0].end + sp
			} else {
				to = max(to, spans[0].end)
			}
		}
	}
	var out []Segment
	push := func(s string, m bool) {
		if s = whitespace.ReplaceAllString(s, " "); s == "" {
			return
		}
		if n := len(out); n > 0 && out[n-1].Match == m {
			out[n-1].Text += s
			return
		}
		out = append(out, Segment{Text: s, Match: m})
	}
	at := from
	for _, sp := range spans {
		if sp.start < from || sp.end > to {
			continue
		}
		push(text[at:sp.start], false)
		push(text[sp.start:sp.end], true)
		at = sp.end
	}
	push(text[at:to], false)
	if len(out) == 0 {
		return nil
	}
	out[0].Text = strings.TrimLeft(out[0].Text, " ")
	out[len(out)-1].Text = strings.TrimRight(out[len(out)-1].Text, " ")
	if from > 0 {
		out = append([]Segment{{Text: "… "}}, out...)
	}
	if to < len(text) {
		out = append(out, Segment{Text: " …"})
	}
	return out
}

// Correct rewrites query with each corrected word replaced, for a "did you mean" offer.
func Correct(query string, corrections map[string]string) string {
	if len(corrections) == 0 {
		return ""
	}
	byNormal := map[string]string{}
	for w, fix := range corrections {
		byNormal[normal(w)] = fix
	}
	var out strings.Builder
	at := 0
	words(query, func(w string, start, end int) {
		if fix, ok := byNormal[normal(w)]; ok {
			out.WriteString(query[at:start])
			out.WriteString(fix)
			at = end
		}
	})
	out.WriteString(query[at:])
	if s := out.String(); s != query {
		return s
	}
	return ""
}

// Relation is an artifact like another, with the words they most share.
type Relation struct {
	ID     string   `json:"-"`
	Score  float64  `json:"score"`
	Shared []string `json:"shared,omitempty"`
}

// Related finds up to n artifacts that share the most telling vocabulary with id.
func (ix *Index) Related(id string, n int) []Relation {
	i, ok := ix.byID[id]
	if !ok {
		return nil
	}
	v := ix.vec[i]
	var out []Relation
	for j, other := range ix.vec {
		if j == i {
			continue
		}
		type kv struct {
			t string
			w float64
		}
		var score float64
		var shared []kv
		for t, w := range v {
			if o := other[t]; o > 0 {
				score += w * o
				shared = append(shared, kv{t, w * o})
			}
		}
		if score < 0.05 {
			continue
		}
		sort.Slice(shared, func(a, c int) bool {
			return shared[a].w > shared[c].w || shared[a].w == shared[c].w && shared[a].t < shared[c].t
		})
		r := Relation{ID: ix.docs[j].ID, Score: math.Round(score*1000) / 1000}
		for _, s := range shared {
			if len(r.Shared) == 3 {
				break
			}
			if w := ix.surface[s.t]; len([]rune(w)) >= 3 {
				r.Shared = append(r.Shared, w)
			}
		}
		out = append(out, r)
	}
	sort.Slice(out, func(a, c int) bool {
		return out[a].Score > out[c].Score || out[a].Score == out[c].Score && out[a].ID < out[c].ID
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// SuggestTags proposes up to n tags for id from the words that most set it apart,
// preferring tags the library already uses.
func (ix *Index) SuggestTags(id string, n int) []string {
	i, ok := ix.byID[id]
	if !ok {
		return nil
	}
	d := ix.docs[i]
	type kv struct {
		tag string
		w   float64
	}
	var all []kv
	have := map[string]bool{}
	for _, t := range d.Tags {
		have[t] = true
	}
	kindTerms := map[string]bool{}
	words(d.text[fKind], func(w string, _, _ int) { kindTerms[term(w)] = true })
	for t, w := range ix.vec[i] {
		readable := false
		for _, f := range []field{fTitle, fDescription, fHeadings, fBody, fComments} {
			readable = readable || d.tf[f][t] > 0
		}
		if !readable || kindTerms[t] {
			continue
		}
		tag := Tag(ix.surface[t])
		if existing, ok := ix.tags[t]; ok {
			tag, w = existing, w*2
		}
		if len([]rune(tag)) < 3 || have[tag] || !plainWord(tag) {
			continue
		}
		all = append(all, kv{tag, w})
	}
	sort.Slice(all, func(a, c int) bool { return all[a].w > all[c].w || all[a].w == all[c].w && all[a].tag < all[c].tag })
	var out []string
	for _, e := range all {
		if len(out) == n {
			break
		}
		if !contains(out, e.tag) {
			out = append(out, e.tag)
		}
	}
	return out
}
