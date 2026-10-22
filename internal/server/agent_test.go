package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/LaneBirmingham/shartifacts/internal/store"
)

func TestAgentTools(t *testing.T) {
	h := testServer(t)
	call := func(name string, args map[string]any) map[string]any {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
		return mcpRequest(t, h, "http://127.0.0.1:8080", string(b))
	}
	sc := func(r map[string]any) map[string]any { m, _ := r["structuredContent"].(map[string]any); return m }
	id := sc(call("create_artifact", map[string]any{"title": "Board", "kind": "html", "content": "x", "capabilities": map[string]any{"db": map[string]any{"rules": []any{map[string]any{"path": "notes", "write": "owner"}}}}}))["id"].(string)
	call("artifact_db", map[string]any{"id": id, "op": "batch", "batch": []any{
		map[string]any{"op": "set", "path": "tasks/t1", "data": map[string]any{"title": "Ship it", "n": 1}},
		map[string]any{"op": "set", "path": "tasks/t2", "data": map[string]any{"title": "Test it", "n": 2}},
		map[string]any{"op": "str_replace", "path": "tasks/t2", "old_str": "Test it", "new_str": "Tested"},
	}})
	got := sc(call("artifact_db", map[string]any{"id": id, "op": "query", "collection": "tasks", "where": []any{[]any{"n", ">", 1}}}))
	docs := got["docs"].([]any)
	if len(docs) != 1 || docs[0].(map[string]any)["data"].(map[string]any)["title"] != "Tested" {
		t.Fatal(got)
	}
	if r := call("artifact_db", map[string]any{"id": id, "op": "get", "path": "tasks"}); r["isError"] != true {
		t.Fatal("collection path accepted for get")
	}
	// watch_artifacts sees the version and data changes, and waits for the next one.
	first := sc(call("watch_artifacts", map[string]any{"ids": []string{id}}))
	if len(first["events"].([]any)) < 3 {
		t.Fatal(first)
	}
	cursor := first["cursor"].(float64)
	go func() {
		time.Sleep(200 * time.Millisecond)
		request(h, "POST", "/api/artifacts/"+id+"/comments", `{"body":"Looks good"}`)
	}()
	start := time.Now()
	next := sc(call("watch_artifacts", map[string]any{"ids": []string{id}, "after": cursor, "wait_seconds": 5}))
	ev := next["events"].([]any)
	if len(ev) != 1 || ev[0].(map[string]any)["kind"] != "comments" || time.Since(start) > 3*time.Second {
		t.Fatal(next)
	}
	// delete_artifact needs the confirmation token.
	asked := sc(call("delete_artifact", map[string]any{"id": id, "base_version": 1}))
	if asked["confirm"] == nil || !strings.Contains(asked["message"].(string), "confirm") {
		t.Fatal(asked)
	}
	if r := call("delete_artifact", map[string]any{"id": id, "base_version": 1, "confirm": "nope"}); r["isError"] != true {
		t.Fatal("deleted without the token")
	}
	if done := sc(call("delete_artifact", map[string]any{"id": id, "base_version": 1, "confirm": asked["confirm"]})); done["deleted"] != true {
		t.Fatal(done)
	}
	if w := request(h, "GET", "/api/artifacts/"+id, ""); w.Code != 404 {
		t.Fatal("still there")
	}
}

func TestPublishEndpoint(t *testing.T) {
	h := testServer(t)
	w := request(h, "POST", "/api/artifacts", `{"title":"Site","kind":"html","content":"<!doctype html><p>1","files":{"a.json":"{}","b.json":"[]"}}`)
	var a store.Artifact
	json.Unmarshal(w.Body.Bytes(), &a)
	pub := func(body string) (int, map[string]any) {
		w := request(h, "POST", "/api/artifacts/"+a.ID+"/publish", body)
		var out map[string]any
		json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	if code, out := pub(`{"base_version":1,"html":"<p>no doctype"}`); code != 400 || out["code"] != "invalid_content" {
		t.Fatal(code, out)
	}
	if code, out := pub(`{"base_version":1,"html":"<!doctype html><p>2"}`); code != 200 || out["version"] != "2" {
		t.Fatal(code, out)
	}
	if code, out := pub(`{"base_version":1,"html":"<!doctype html><p>stale"}`); code != 409 || out["code"] != "conflict" || out["live"] != "2" {
		t.Fatal(code, out)
	}
	// Per-file preconditions: a writer on version 1 saving b.json still lands.
	if code, out := pub(`{"base_version":1,"files":{"b.json":{"content":"[1]","if_match":"` + sha("[]") + `","checked":true}}}`); code != 200 || out["version"] != "3" || out["shas"].(map[string]any)["b.json"] != sha("[1]") {
		t.Fatal(code, out)
	}
	code, out := pub(`{"base_version":3,"files":{"b.json":{"content":"[2]","if_match":"` + sha("[]") + `","checked":true},"new.json":{"content":"1","if_match":null,"checked":true}}}`)
	paths, _ := out["paths"].([]any)
	if code != 409 || len(paths) != 1 || paths[0].(map[string]any)["path"] != "b.json" {
		t.Fatal(code, out)
	}
	if code, out = pub(`{"base_version":3,"files":{"a.json":null}}`); code != 200 {
		t.Fatal(code, out)
	}
	w = request(h, "GET", "/api/artifacts/"+a.ID, "")
	json.Unmarshal(w.Body.Bytes(), &a)
	last := a.Revisions[len(a.Revisions)-1]
	if _, ok := last.Files["a.json"]; ok || last.Files["b.json"] != "[1]" || last.Content != "<!doctype html><p>2" {
		t.Fatal(last)
	}
}

func TestSampleValidation(t *testing.T) {
	c := NewClaude("k", "claude-opus-5-5")
	for _, bad := range []string{
		`{"messages":[]}`,
		`{"messages":[{"role":"system","content":[{"type":"text","text":"x"}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://x"}}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"server_tool_use","id":"1","name":"web_search","input":{}}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"text","text":"x"}]}],"tools":[{"type":"web_search_20260209","name":"web_search"}]}`,
		`{"messages":[{"role":"user","content":[{"type":"text","text":"x"}]}],"model_tier":"huge"}`,
	} {
		var in sampleRequest
		json.Unmarshal([]byte(bad), &in)
		if _, _, err := c.params(in); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	var in sampleRequest
	json.Unmarshal([]byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"tools":[{"name":"get_total","description":"d","input_schema":{"type":"object","properties":{}}}],"model_tier":"quick","effort":"low","json":true}`), &in)
	p, tier, err := c.params(in)
	if err != nil || tier != "quick" || p.Model != "claude-haiku-5-5" || len(p.Tools) != 1 || p.OutputConfig.Effort != "low" || !strings.Contains(p.System[0].Text, "JSON") {
		t.Fatal(err, tier, p.Model)
	}
}
