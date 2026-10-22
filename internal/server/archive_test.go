package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/LaneBirmingham/shartifacts/internal/store"
)

// create makes an artifact over HTTP and returns its ID.
func create(t *testing.T, h http.Handler, title, content string) string {
	t.Helper()
	w := request(h, "POST", "/api/artifacts", `{"title":"`+title+`","kind":"html","content":"`+content+`"}`)
	var a store.Artifact
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &a) != nil {
		t.Fatal(w.Code, w.Body.String())
	}
	return a.ID
}

func titles(t *testing.T, body []byte) string {
	t.Helper()
	var list []store.Summary
	if e := json.Unmarshal(body, &list); e != nil {
		t.Fatal(e, string(body))
	}
	var out []string
	for _, a := range list {
		out = append(out, a.Title)
	}
	return strings.Join(out, ",")
}

func TestArchiveOverHTTP(t *testing.T) {
	h := testServer(t)
	keep := create(t, h, "Keep", "budget")
	old := create(t, h, "Old", "budget draft")
	bulk := func(body string) (int, bulkResult) {
		w := request(h, "POST", "/api/artifacts/bulk", body)
		var out bulkResult
		json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	if code, out := bulk(`{"op":"archive","ids":["` + old + `","` + old + `","missing"]}`); code != 200 || strings.Join(out.Done, ",") != old || len(out.Failed) != 1 || out.Failed[0].ID != "missing" {
		t.Fatal(code, out)
	}
	for query, want := range map[string]string{"": "Keep", "?archived=only": "Old", "?archived=include": "Old,Keep"} {
		if w := request(h, "GET", "/api/artifacts"+query, ""); titles(t, w.Body.Bytes()) != want {
			t.Fatal(query, w.Body.String())
		}
	}
	if w := request(h, "GET", "/api/artifacts?archived=everything", ""); w.Code != 400 {
		t.Fatal(w.Code)
	}
	// Search leaves the archive out unless asked.
	search := func(query string) string {
		var res searchResponse
		json.Unmarshal(request(h, "GET", "/api/search?"+query, "").Body.Bytes(), &res)
		var out []string
		for _, r := range res.Results {
			out = append(out, r.Title)
		}
		return strings.Join(out, ",")
	}
	for query, want := range map[string]string{"q=budget": "Keep", "q=budget&archived=only": "Old", "q=is:archived": "Old", "q=budget&archived=include&sort=oldest": "Keep,Old"} {
		if got := search(query); got != want {
			t.Errorf("%s: got %q, want %q", query, got, want)
		}
	}
	// The archive is never suggested as related.
	var rel struct {
		Related []relatedResult `json:"related"`
	}
	json.Unmarshal(request(h, "GET", "/api/artifacts/"+keep+"/related", "").Body.Bytes(), &rel)
	for _, r := range rel.Related {
		if r.Summary.ID == old {
			t.Fatal("related suggests the archive")
		}
	}
	// Bulk delete takes only archived artifacts; the library is left alone.
	if code, out := bulk(`{"op":"delete","ids":["` + keep + `","` + old + `"]}`); code != 200 || strings.Join(out.Done, ",") != old || len(out.Failed) != 1 || out.Failed[0].ID != keep || !strings.Contains(out.Failed[0].Error, "not archived") {
		t.Fatal(code, out)
	}
	if w := request(h, "GET", "/api/artifacts/"+keep, ""); w.Code != 200 {
		t.Fatal("deleted a library artifact in bulk")
	}
	if w := request(h, "GET", "/api/artifacts/"+old, ""); w.Code != 404 {
		t.Fatal("archived artifact survived")
	}
	for _, body := range []string{`{"op":"purge","ids":["` + keep + `"]}`, `{"op":"archive","ids":[]}`, `{"op":"archive","ids":[` + strings.Repeat(`"x",`, maxBulk) + `"x"]}`} {
		if code, _ := bulk(body); code != 400 {
			t.Fatal(body[:30], code)
		}
	}
	// Restoring brings an artifact back to the library.
	bulk(`{"op":"archive","ids":["` + keep + `"]}`)
	if code, out := bulk(`{"op":"restore","ids":["` + keep + `"]}`); code != 200 || len(out.Done) != 1 {
		t.Fatal(code, out)
	}
	if w := request(h, "GET", "/api/artifacts", ""); titles(t, w.Body.Bytes()) != "Keep" {
		t.Fatal(w.Body.String())
	}
}

func TestArchiveTools(t *testing.T) {
	h := testServer(t)
	call := func(name string, args map[string]any) map[string]any {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
		return mcpRequest(t, h, "http://127.0.0.1:8080", string(b))
	}
	sc := func(r map[string]any) map[string]any { m, _ := r["structuredContent"].(map[string]any); return m }
	var ids []string
	for _, title := range []string{"Test page", "Scratch", "Roadmap"} {
		ids = append(ids, sc(call("create_artifact", map[string]any{"title": title, "kind": "markdown", "content": "# " + title + " notes"}))["id"].(string))
	}
	listed := func(archived string) (string, map[string]any) {
		out := sc(call("list_artifacts", map[string]any{"archived": archived}))
		var got []string
		for _, a := range out["artifacts"].([]any) {
			got = append(got, a.(map[string]any)["title"].(string))
		}
		return strings.Join(got, ","), out
	}
	done := sc(call("archive_artifacts", map[string]any{"ids": ids[:2]}))
	if len(done["done"].([]any)) != 2 || len(done["failed"].([]any)) != 0 {
		t.Fatal(done)
	}
	if got, out := listed(""); got != "Roadmap" || out["total"] != float64(1) || out["archived"] != float64(2) {
		t.Fatal(got, out)
	}
	if got, out := listed("only"); got != "Scratch,Test page" || out["artifacts"].([]any)[0].(map[string]any)["archived_at"] == nil || out["archived"] != nil {
		t.Fatal(got, out)
	}
	if r := call("list_artifacts", map[string]any{"archived": "yes please"}); r["isError"] != true {
		t.Fatal("accepted a bad filter")
	}
	found := func(args map[string]any) int { return len(sc(call("search_artifacts", args))["results"].([]any)) }
	if n := found(map[string]any{"query": "notes"}); n != 1 {
		t.Fatal("search found the archive", n)
	}
	if n := found(map[string]any{"query": "notes", "archived": "include"}); n != 3 {
		t.Fatal(n)
	}
	if res := sc(call("search_artifacts", map[string]any{"query": "scratch", "archived": "only"}))["results"].([]any); len(res) != 1 || res[0].(map[string]any)["archived_at"] == nil {
		t.Fatal(res)
	}
	// Restoring and saving both say where the artifact stands.
	sc(call("archive_artifacts", map[string]any{"ids": ids[1:2], "restore": true}))
	if got, _ := listed(""); got != "Roadmap,Scratch" {
		t.Fatal(got)
	}
	if saved := sc(call("update_artifact", map[string]any{"id": ids[0], "base_version": 1, "note": "touch"})); saved["archived_at"] == nil {
		t.Fatal(saved)
	}
	// Archived artifacts are deleted without a token, alone or together; one still in the
	// library is refused in bulk and still needs the person's confirmation alone.
	res := sc(call("delete_artifact", map[string]any{"ids": []string{ids[0], ids[2]}}))
	if len(res["done"].([]any)) != 1 || res["failed"].([]any)[0].(map[string]any)["id"] != ids[2] {
		t.Fatal(res)
	}
	if asked := sc(call("delete_artifact", map[string]any{"id": ids[2], "base_version": 1})); asked["confirm"] == nil || asked["deleted"] != nil {
		t.Fatal("deleted a library artifact without confirmation", asked)
	}
	sc(call("archive_artifacts", map[string]any{"ids": ids[2:]}))
	if gone := sc(call("delete_artifact", map[string]any{"id": ids[2]})); gone["deleted"] != true {
		t.Fatal(gone)
	}
	if r := call("delete_artifact", map[string]any{"id": ids[1], "ids": ids[1:2]}); r["isError"] != true {
		t.Fatal("accepted id and ids")
	}
	if got, _ := listed("include"); got != "Scratch" {
		t.Fatal(got)
	}
}
