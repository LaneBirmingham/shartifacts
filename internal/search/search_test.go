package search

import (
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

func library() (*Index, map[string]Meta) {
	sources := []Source{
		{ID: "budget", Kind: "react", Title: "Household budget", Description: "Monthly spending against the plan", Tags: []string{"finance", "home"},
			Content: `import { BarChart } from "recharts"
export default function App() { const rows = [{ name: "Groceries", value: 420 }]; return <div className="flex gap-2"><h1>Where the money went</h1><p>Mortgage repayments and groceries this month.</p></div> }`},
		{ID: "mortgage", Kind: "html", Title: "Refinance calculator", Tags: []string{"finance"},
			Content: `<!doctype html><title>Loan maths</title><h1>Refinance calculator</h1><p>Compare two Suncorp loans and the repayment schedule.</p><input placeholder="Interest rate"><script>const label = "Total interest paid"</script>`},
		{ID: "recipes", Kind: "markdown", Title: "Weeknight recipes", Content: "# Dinners\n\n**Lemon pasta** with garlic.\n\n## Pokémon cookies\n\nShaped like Pikachu.\n\n```js\nconst secret = 'oven'\n```", Comments: []string{"Sam: add the lasagne one"}},
		{ID: "deck", Kind: "slides", Title: "Platform roadmap", Content: "# Platform roadmap\n\nReliability first\n\n---\n\n# Observability\n\nDashboards for every service\n\nNote: mention the on-call rota", Notes: []string{"Added the observability slide"}},
		{ID: "flow", Kind: "mermaid", Title: "Deploy pipeline", Content: "flowchart LR\n  Build --> Test --> Deploy"},
		{ID: "clip", Kind: "html", Title: "Holiday clip", Content: `<video controls src="/media/abc123.mp4"></video><p>Beach trip</p>`},
		{ID: "draft", Kind: "html", Title: "Packing list", Tags: []string{"travel"}, Content: `<p>Towels, sunscreen and the Suncorp policy number.</p>`},
	}
	var docs []*Doc
	for _, s := range sources {
		docs = append(docs, NewDoc(s))
	}
	meta := map[string]Meta{
		"budget":   {Pinned: true, Created: now.AddDate(0, -3, 0), Updated: now.Add(-2 * time.Hour)},
		"mortgage": {Created: now.AddDate(0, -2, 0), Updated: now.AddDate(0, 0, -10)},
		"recipes":  {Created: now.AddDate(-1, 0, 0), Updated: now.AddDate(0, -2, 0), Comments: 1, OpenComments: 1},
		"deck":     {Created: now.AddDate(0, 0, -3), Updated: now.AddDate(0, 0, -3)},
		"flow":     {Created: now.AddDate(0, 0, -40), Updated: now.AddDate(0, 0, -40)},
		"clip":     {Created: now.AddDate(0, 0, -1), Updated: now.AddDate(0, 0, -1)},
		"draft":    {Archived: true, Created: now.AddDate(0, -4, 0), Updated: now.AddDate(0, -3, 0)},
	}
	return Build(docs), meta
}

func find(t *testing.T, query string) Results {
	t.Helper()
	ix, meta := library()
	return ix.Search(Parse(query, now), meta, Options{Now: now})
}

func ids(r Results) string {
	var out []string
	for _, h := range r.Hits {
		out = append(out, h.ID)
	}
	return strings.Join(out, ",")
}

func first(t *testing.T, query, want string) Results {
	t.Helper()
	r := find(t, query)
	if len(r.Hits) == 0 || r.Hits[0].ID != want {
		t.Fatalf("%q: got [%s], want %s first", query, ids(r), want)
	}
	return r
}

func TestFindsByContentTyposAndWordForms(t *testing.T) {
	first(t, "budget", "budget")
	first(t, "groceries", "budget") // rendered text in a React component
	first(t, "grocery", "budget")   // word forms meet
	first(t, "bugdet", "budget")    // transposed letters
	// A missing letter: the budget mentions mortgage repayments, and loans are mortgages.
	if got := ids(find(t, "mortage")); got != "mortgage,budget" {
		t.Errorf("mortage: got [%s]", got)
	}
	first(t, "refinanse", "mortgage")
	first(t, "suncorp loan", "mortgage")
	first(t, "interest rate", "mortgage") // placeholder text
	first(t, "pokemon", "recipes")        // accents fold
	first(t, "lasagne", "recipes")        // comments
	first(t, "observability", "deck")
	first(t, "dashbo", "deck") // the last word is a prefix while typing
	first(t, "pipeline", "flow")
	first(t, "presentation", "deck") // what it is, not what it says
	first(t, "video", "clip")        // what it contains
	first(t, "movie", "clip")        // synonyms
}

func TestCorrectsTypos(t *testing.T) {
	r := find(t, "refinanse calculater")
	if got := Correct("refinanse calculater", r.Corrections); got != "refinance calculator" {
		t.Fatalf("suggestion %q", got)
	}
	r = find(t, "suncrop lons")
	if got := Correct("suncrop lons", r.Corrections); got != "suncorp loans" || len(r.Hits) == 0 || r.Hits[0].ID != "mortgage" {
		t.Fatalf("suggestion %q for [%s]", got, ids(r))
	}
	if r := find(t, "budget"); len(r.Corrections) != 0 {
		t.Fatalf("a word that matches needs no correction: %v", r.Corrections)
	}
}

func TestNoNoiseFromSource(t *testing.T) {
	// Class names, imports and code-block identifiers are not prose.
	if r := find(t, "flex"); len(r.Hits) > 0 && r.Hits[0].Matched[0] != "source" {
		t.Fatalf("flex matched %v", r.Hits[0].Matched)
	}
	r := find(t, "zebra")
	if len(r.Hits) != 0 {
		t.Fatalf("zebra matched [%s]", ids(r))
	}
}

func TestFiltersAndPhrases(t *testing.T) {
	cases := map[string]string{
		"kind:html":             "clip,mortgage",
		"type:deck":             "deck",
		"#finance":              "budget,mortgage",
		"tag:finance kind:html": "mortgage",
		"is:pinned":             "budget",
		"has:comments":          "recipes",
		"has:open":              "recipes",
		"has:video":             "clip",
		"has:chart":             "budget",
		"has:form":              "mortgage",
		"has:diagram":           "flow",
		"updated:week":          "budget,clip,deck",
		"updated:today":         "budget",
		"created:2026-10":       "clip,deck",
		"before:month":          "flow,recipes",
		"after:2026-10-14":      "budget,clip,deck",
		`"lemon pasta"`:         "recipes",
		`"pasta lemon"`:         "",
		"finance -suncorp":      "budget",
		// The archive stays out of every search unless one asks for it.
		"suncorp":                    "mortgage",
		"is:archived":                "draft",
		"is:archived suncorp":        "draft",
		"archived:include kind:html": "clip,mortgage,draft",
		"archived:include #travel":   "draft",
		"is:archived archived:no":    "budget,clip,deck,mortgage,flow,recipes",
	}
	for q, want := range cases {
		if got := ids(find(t, q)); got != want {
			t.Errorf("%q: got [%s], want [%s]", q, got, want)
		}
	}
}

func TestOldestFirst(t *testing.T) {
	ix, meta := library()
	r := ix.Search(Parse("archived:include", now), meta, Options{Sort: "oldest", Now: now})
	if got := ids(r); got != "draft,recipes,flow,mortgage,deck,clip,budget" {
		t.Fatal(got)
	}
}

func TestUnknownOperatorsAreWords(t *testing.T) {
	q := Parse("note:hello 10:30", now)
	if len(q.Words) != 4 || q.filtered() {
		t.Fatalf("parsed %+v", q)
	}
}

func TestSnippetHighlights(t *testing.T) {
	r := first(t, "suncorp", "mortgage")
	var marked []string
	var text strings.Builder
	for _, s := range r.Hits[0].Snippet {
		text.WriteString(s.Text)
		if s.Match {
			marked = append(marked, s.Text)
		}
	}
	if len(marked) != 1 || marked[0] != "Suncorp" || !strings.Contains(text.String(), "Compare two Suncorp loans") {
		t.Fatalf("snippet %q marks %v", text.String(), marked)
	}
	r = first(t, "budget", "budget")
	if len(r.Hits[0].TitleSegments) != 2 || !r.Hits[0].TitleSegments[1].Match || r.Hits[0].TitleSegments[1].Text != "budget" {
		t.Fatalf("title segments %+v", r.Hits[0].TitleSegments)
	}
}

func TestRelatedAndTags(t *testing.T) {
	ix, _ := library()
	rel := ix.Related("mortgage", 3)
	if len(rel) == 0 || rel[0].ID != "budget" {
		t.Fatalf("related %+v", rel)
	}
	tags := ix.SuggestTags("mortgage", 5)
	if len(tags) == 0 || contains(tags, "finance") {
		t.Fatalf("suggested %v", tags)
	}
	if !contains(tags, "refinance") && !contains(tags, "loan") {
		t.Fatalf("suggested %v", tags)
	}
}

func TestStemAndDistance(t *testing.T) {
	for in, want := range map[string]string{"charts": "chart", "planning": "plan", "planned": "plan", "stories": "stori", "story": "stori", "recipes": "recip", "recipe": "recip", "making": "mak", "make": "mak", "calling": "call", "glass": "glass"} {
		if got := stem(in); got != want {
			t.Errorf("stem(%s) = %s, want %s", in, got, want)
		}
	}
	if d := distance("bugdet", "budget", 2); d != 1 {
		t.Errorf("transposition distance %d", d)
	}
	if d := distance("kitten", "sitting", 2); d != 3 {
		t.Errorf("distance beyond the limit %d", d)
	}
}
