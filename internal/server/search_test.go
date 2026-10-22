package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/LaneBirmingham/shartifacts/internal/search"
	"github.com/LaneBirmingham/shartifacts/internal/store"
)

func TestSearchOverHTTPAndMCP(t *testing.T) {
	h := testServer(t)
	create := func(body string) store.Artifact {
		t.Helper()
		w := request(h, "POST", "/api/artifacts", body)
		var a store.Artifact
		if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &a) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return a
	}
	budget := create(`{"title":"Household budget","kind":"html","content":"<h1>Spending</h1><p>Mortgage repayments and groceries.</p>","tags":["Finance"," #Home ","finance"]}`)
	loan := create(`{"title":"Refinance calculator","kind":"html","content":"<p>Compare two mortgage loans and their repayments.</p>","tags":["finance"]}`)
	create(`{"title":"Weeknight recipes","kind":"markdown","content":"# Dinners\n\nLemon pasta."}`)
	if strings.Join(budget.Tags, ",") != "finance,home" {
		t.Fatal("tags not normalised:", budget.Tags)
	}
	if w := request(h, "POST", "/api/artifacts", `{"title":"x","kind":"html","content":"x","tags":["a,b"]}`); w.Code != 400 {
		t.Fatal("bad tag accepted", w.Body.String())
	}

	search := func(query string) searchResponse {
		t.Helper()
		w := request(h, "GET", "/api/search?"+query, "")
		var res searchResponse
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &res) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return res
	}
	res := search("q=grocries")
	if len(res.Results) != 1 || res.Results[0].Summary.ID != budget.ID || res.Suggestion != "groceries" {
		t.Fatalf("%+v", res)
	}
	if !strings.Contains(fmtSegments(res.Results[0].Snippet), "[groceries]") {
		t.Fatal(res.Results[0].Snippet)
	}
	if res = search("q=&tag=finance&sort=title"); len(res.Results) != 2 || res.Results[0].Title != "Household budget" {
		t.Fatalf("%+v", res)
	}
	if res = search("q=pasta&kind=html"); len(res.Results) != 0 {
		t.Fatalf("%+v", res)
	}

	// Tags change without a new version, and the index follows.
	w := request(h, "PUT", "/api/artifacts/"+loan.ID, `{"base_version":1,"tags":["finance","loans"],"edits":[]}`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if res = search("q=%23loans"); len(res.Results) != 1 || res.Results[0].Summary.ID != loan.ID {
		t.Fatalf("%+v", res)
	}

	w = request(h, "GET", "/api/artifacts/"+loan.ID+"/related", "")
	var rel struct {
		Related []relatedResult `json:"related"`
		Tags    []string        `json:"suggested_tags"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &rel) != nil || len(rel.Related) == 0 || rel.Related[0].Summary.ID != budget.ID || len(rel.Tags) == 0 {
		t.Fatal(w.Body.String())
	}
	if w = request(h, "GET", "/api/artifacts/nope/related", ""); w.Code != 404 {
		t.Fatal(w.Code)
	}

	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "search_artifacts", "arguments": map[string]any{"query": "refinanse"}}})
	out, _ := json.Marshal(mcpRequest(t, h, "http://127.0.0.1:8080", string(b))["structuredContent"])
	if !strings.Contains(string(out), `"did_you_mean":"refinance"`) || !strings.Contains(string(out), `"id":"`+loan.ID+`"`) {
		t.Fatal(string(out))
	}
	b, _ = json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "create_artifact", "arguments": map[string]any{"title": "Trip", "kind": "markdown", "content": "Beach", "tags": []string{"Travel"}}}})
	id := mcpRequest(t, h, "http://127.0.0.1:8080", string(b))["structuredContent"].(map[string]any)["id"].(string)
	if res = search("q=tag:travel"); len(res.Results) != 1 || res.Results[0].Summary.ID != id {
		t.Fatalf("%+v", res)
	}
}

func fmtSegments(segments []search.Segment) string {
	var out strings.Builder
	for _, s := range segments {
		if s.Match {
			out.WriteString("[" + s.Text + "]")
		} else {
			out.WriteString(s.Text)
		}
	}
	return out.String()
}
