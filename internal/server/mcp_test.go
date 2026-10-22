package server

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/LaneBirmingham/shartifacts/internal/store"
)

func mcpRequest(t *testing.T, h http.Handler, origin, body string) map[string]any {
	t.Helper()
	r := httptest.NewRequest("POST", origin+"/mcp", strings.NewReader(body))
	r.RemoteAddr = "192.168.1.9:1234"
	if strings.HasPrefix(origin, "https:") {
		r.TLS = &tls.ConnectionState{}
	}
	// Never let proxy-supplied headers override the validated connection address.
	r.Header.Set("X-Forwarded-Host", "wrong.example:5000")
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("MCP %d: %s", w.Code, w.Body.String())
	}
	var out map[string]any
	if e := json.Unmarshal(w.Body.Bytes(), &out); e != nil {
		t.Fatal(e)
	}
	if out["error"] != nil {
		t.Fatal(out)
	}
	return out["result"].(map[string]any)
}

func TestMCPUploadDiscoveryPerConnection(t *testing.T) {
	h := testServer(t)
	for _, origin := range []string{"http://192.168.1.2:8080", "http://[fd12::2]:9090", "https://myserver.tailnet.ts.net:8443", "http://100.101.102.103:8081", "http://127.0.0.1:8080"} {
		t.Run(origin, func(t *testing.T) {
			init := mcpRequest(t, h, origin, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
			instructions := init["instructions"].(string)
			for _, part := range []string{origin + "/api/media", "list_media", "get_design_system", "Do not manually transcribe", "Explicit user design instructions always override", "Preserve an existing artifact's design"} {
				if !strings.Contains(instructions, part) {
					t.Fatalf("missing guidance %q", part)
				}
			}
			info := mcpRequest(t, h, origin, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_media","arguments":{}}}`)["structuredContent"].(map[string]any)
			if info["upload_url"] != origin+"/api/media" || info["base_url"] != origin || info["method"] != "POST" || info["max_file_bytes"] != float64(store.MaxMedia) {
				t.Fatal(info)
			}
			if !strings.Contains(info["curl_example"].(string), "--url '"+origin+"/api/media'") {
				t.Fatal(info)
			}
			begun := mcpRequest(t, h, origin, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"begin_media_upload","arguments":{"mime_type":"video/mp4"}}}`)["structuredContent"].(map[string]any)
			if begun["upload_url"] != info["upload_url"] || begun["curl_example"] != info["curl_example"] || begun["offset"] != float64(0) || begun["upload_id"] == "" {
				t.Fatal(begun)
			}
			listed := mcpRequest(t, h, origin, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"list_media","arguments":{}}}`)["structuredContent"].(map[string]any)
			if listed["upload_url"] != info["upload_url"] {
				t.Fatal(listed)
			}
			tools := mcpRequest(t, h, origin, `{"jsonrpc":"2.0","id":5,"method":"tools/list"}`)
			for _, tool := range tools["tools"].([]any) {
				item := tool.(map[string]any)
				if item["name"] == "begin_media_upload" && !strings.Contains(item["description"].(string), origin+"/api/media") {
					t.Fatal(item)
				}
			}
		})
	}
}

// Agents connect by LAN address, but the links they hand people use -public-origin; the
// upload URL stays on the connected address because the agent's shell uses it.
func TestMCPLinksUsePublicOrigin(t *testing.T) {
	const lan, public = "http://192.168.1.20:8080", "https://shartifacts.example.com"
	for _, tc := range []struct{ public, want string }{{public + "/", public}, {"", lan}} {
		s, h := appServer(t, nil)
		s.PublicOrigin = tc.public
		init := mcpRequest(t, h, lan, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
		instructions := init["instructions"].(string)
		if !strings.Contains(instructions, "Give people links on "+tc.want+", e.g. "+tc.want+"/a/{id}") || !strings.Contains(instructions, "Upload URL: "+lan+"/api/media") {
			t.Fatalf("public %q: %s", tc.public, instructions)
		}
		res := mcpRequest(t, h, lan, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"deploy_app","arguments":{"name":"Links","files":{"index.html":"<h1>hi</h1>"}}}}`)["structuredContent"].(map[string]any)
		id := res["id"].(string)
		if res["hub_url"] != tc.want+"/apps/"+id || res["preview_url"] != tc.want+"/run/"+id+"/1/" || res["live_url"] != tc.want+"/run/"+id+"/live/" {
			t.Fatalf("public %q: %v", tc.public, res)
		}
		got := mcpRequest(t, h, lan, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_app","arguments":{"id":"`+id+`"}}}`)["structuredContent"].(map[string]any)
		if got["hub_url"] != tc.want+"/apps/"+id {
			t.Fatalf("public %q: %v", tc.public, got)
		}
	}
}

// The instructions are a summary; the full guide is a resource an agent reads on demand.
// It must be listed, read in full with this connection's links and upload URL, and
// refuse URIs it does not serve.
func TestMCPGuideResource(t *testing.T) {
	h := testServer(t)
	const origin = "http://192.168.1.20:8080"
	init := mcpRequest(t, h, origin, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	if init["capabilities"].(map[string]any)["resources"] == nil {
		t.Fatal("resources capability not advertised", init["capabilities"])
	}
	instructions := init["instructions"].(string)
	if !strings.Contains(instructions, guideURI) {
		t.Fatal("instructions do not point at the guide", instructions)
	}

	listed := mcpRequest(t, h, origin, `{"jsonrpc":"2.0","id":2,"method":"resources/list"}`)["resources"].([]any)
	if len(listed) != 1 {
		t.Fatal(listed)
	}
	if r := listed[0].(map[string]any); r["uri"] != guideURI || r["name"] != "guide" || r["mimeType"] != "text/plain" || r["description"] == "" {
		t.Fatal(r)
	}

	contents := mcpRequest(t, h, origin, `{"jsonrpc":"2.0","id":3,"method":"resources/read","params":{"uri":"`+guideURI+`"}}`)["contents"].([]any)
	if len(contents) != 1 {
		t.Fatal(contents)
	}
	read := contents[0].(map[string]any)
	guide, _ := read["text"].(string)
	if read["uri"] != guideURI || read["mimeType"] != "text/plain" {
		t.Fatal(read)
	}
	// One phrase from every part of the guide, so none is dropped when the prose is edited.
	for _, part := range []string{
		`slides (Markdown slides separated by --- lines; "Note:" starts speaker notes)`,
		"React must default-export a component",
		"Artifacts get Claude's capability runtime",
		"search_artifacts has:open finds them",
		"update_artifact with archived:true moves artifacts",
		"When the person asks you to tidy up, archive what is stale",
		"delete_artifact with ids permanently deletes archived artifacts",
		"Do not manually transcribe base64 into source (2 MiB limit)",
		"Autoplay needs muted playsinline",
		"Apps are full-stack micro frontends",
		"Before updating, get_artifact for the latest base_version",
		"Give people links on https://shartifacts.example.com, e.g. https://shartifacts.example.com/a/{id}",
		"Explicit user design instructions always override it",
		"Upload URL: " + origin + "/api/media",
		"--url '" + origin + "/api/media'",
	} {
		if !strings.Contains(guide, part) {
			t.Fatalf("guide is missing %q", part)
		}
	}
	// The summary is the short part; the guide carries the rest.
	if len(instructions)*3 > len(guide) {
		t.Fatalf("instructions (%d bytes) are not a summary of the guide (%d bytes)", len(instructions), len(guide))
	}

	r := httptest.NewRequest("POST", origin+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":4,"method":"resources/read","params":{"uri":"shartifacts://other"}}`))
	r.RemoteAddr = "192.168.1.9:1234"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), `"error"`) || strings.Contains(w.Body.String(), "Apps are full-stack") {
		t.Fatalf("unknown resource read: %d %s", w.Code, w.Body.String())
	}
}

func TestMCPDesignSystemUsesWorkspaceCSS(t *testing.T) {
	st, e := store.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	h := (&Server{Store: st, Web: os.DirFS("../../web")}).Handler()
	result := mcpRequest(t, h, "http://127.0.0.1:8080", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_design_system","arguments":{}}}`)
	if result["isError"] == true {
		t.Fatal(result)
	}
	css, e := os.ReadFile("../../web/design-system.css")
	if e != nil {
		t.Fatal(e)
	}
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	if result["structuredContent"] != nil || strings.Count(text, string(css)) != 1 || !strings.Contains(text, "<style>"+string(css)+"</style>") || !strings.Contains(text, "Explicit user instructions take priority") {
		t.Fatal("MCP design differs from workspace CSS")
	}
	if w := request(h, "GET", "/design-system.css", ""); w.Code != 200 || w.Body.String() != string(css) {
		t.Fatal("workspace stylesheet unavailable")
	}
	// Guidance must not mutate user-supplied source.
	source := `<h1 style="color:purple">Custom user design</h1>`
	args, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "create_artifact", "arguments": store.Input{Title: "Custom", Kind: "html", Content: source}}})
	created := mcpRequest(t, h, "http://127.0.0.1:8080", string(args))["structuredContent"].(map[string]any)
	a, e := st.Get(created["id"].(string))
	if e != nil {
		t.Fatal(e)
	}
	if a.Revisions[0].Content != source {
		t.Fatal("default guidance mutated source")
	}
}

func TestMCPUpdateWithEdits(t *testing.T) {
	h := testServer(t)
	const origin = "http://127.0.0.1:8080"
	call := func(name string, args map[string]any) map[string]any {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
		return mcpRequest(t, h, origin, string(b))
	}
	created := call("create_artifact", map[string]any{"title": "Doc", "kind": "markdown", "content": "# One\n\nBody"})["structuredContent"].(map[string]any)
	id := created["id"].(string)
	updated := call("update_artifact", map[string]any{"id": id, "base_version": 1, "edits": []map[string]string{{"old_str": "# One", "new_str": "# Two"}}})
	if updated["isError"] == true || updated["structuredContent"].(map[string]any)["version"] != float64(2) {
		t.Fatal(updated)
	}
	failed := call("update_artifact", map[string]any{"id": id, "base_version": 2, "edits": []map[string]string{{"old_str": "# One", "new_str": "x"}}})
	if failed["isError"] != true || !strings.Contains(failed["content"].([]any)[0].(map[string]any)["text"].(string), "old_str not found") {
		t.Fatal(failed)
	}
	// Title only: the source is kept.
	call("update_artifact", map[string]any{"id": id, "base_version": 2, "title": "Renamed"})
	latest := call("get_artifact", map[string]any{"id": id})["structuredContent"].(map[string]any)["revisions"].([]any)
	r := latest[0].(map[string]any)
	if len(latest) != 1 || r["content"] != "# Two\n\nBody" || r["title"] != "Renamed" || r["kind"] != "markdown" {
		t.Fatal(r)
	}
	first := call("get_artifact", map[string]any{"id": id, "version": 1})["structuredContent"].(map[string]any)["revisions"].([]any)
	if len(first) != 1 || first[0].(map[string]any)["content"] != "# One\n\nBody" {
		t.Fatal(first)
	}
	if missing := call("get_artifact", map[string]any{"id": id, "version": 9}); missing["isError"] != true {
		t.Fatal(missing)
	}
}

func TestStorageOverHTTPAndMCP(t *testing.T) {
	h := testServer(t)
	w := request(h, "POST", "/api/artifacts", `{"title":"Poll","kind":"html","content":"x"}`)
	var a store.Artifact
	json.Unmarshal(w.Body.Bytes(), &a)
	op := func(body string) (int, string) {
		w := request(h, "POST", "/api/artifacts/"+a.ID+"/storage", body)
		return w.Code, strings.TrimSpace(w.Body.String())
	}
	if code, body := op(`{"op":"get","key":"votes"}`); code != 200 || body != "null" {
		t.Fatal(code, body)
	}
	if code, body := op(`{"op":"set","key":"votes","value":"{\"yes\":1}","shared":true}`); code != 200 || body != `{"key":"votes","shared":true,"value":"{\"yes\":1}"}` {
		t.Fatal(code, body)
	}
	if code, _ := op(`{"op":"set","key":"bad key","value":"x"}`); code != 400 {
		t.Fatal("bad key accepted")
	}
	if code, _ := op(`{"op":"set","key":"k"}`); code != 400 {
		t.Fatal("set without value accepted")
	}
	if code, _ := op(`{"op":"drop"}`); code != 400 {
		t.Fatal("unknown op accepted")
	}
	if w := request(h, "POST", "/api/artifacts/nope/storage", `{"op":"list"}`); w.Code != 404 {
		t.Fatal(w.Code)
	}
	call := func(args map[string]any) map[string]any {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "artifact_storage", "arguments": args}})
		return mcpRequest(t, h, "http://127.0.0.1:8080", string(b))
	}
	got := call(map[string]any{"id": a.ID, "op": "get", "key": "votes", "shared": true})["structuredContent"].(map[string]any)
	if got["value"] != `{"yes":1}` {
		t.Fatal(got)
	}
	if got = call(map[string]any{"id": a.ID, "op": "get", "key": "votes"})["structuredContent"].(map[string]any); got["found"] != false {
		t.Fatal("personal scope saw a shared key", got)
	}
	call(map[string]any{"id": a.ID, "op": "set", "key": "note", "value": "hi"})
	if code, body := op(`{"op":"list"}`); code != 200 || body != `{"keys":["note"],"prefix":"","shared":false}` {
		t.Fatal(code, body)
	}
	tools, _ := json.Marshal(mcpRequest(t, h, "http://127.0.0.1:8080", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	if !strings.Contains(string(tools), `"op":{"description":"get, set, delete or list"`) {
		t.Fatal("artifact_storage schema missing its fields")
	}
}

func TestCommentsOverHTTPAndMCP(t *testing.T) {
	h := testServer(t)
	w := request(h, "POST", "/api/artifacts", `{"title":"Page","kind":"html","content":"x"}`)
	var a store.Artifact
	json.Unmarshal(w.Body.Bytes(), &a)
	w = request(h, "POST", "/api/artifacts/"+a.ID+"/comments", `{"body":"Bigger button","quote":"<button>"}`)
	var root store.Comment
	json.Unmarshal(w.Body.Bytes(), &root)
	if w.Code != 201 || root.Author != "you" {
		t.Fatal(w.Body.String())
	}
	call := func(args map[string]any) map[string]any {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "artifact_comments", "arguments": args}})
		return mcpRequest(t, h, "http://127.0.0.1:8080", string(b))
	}
	listed, _ := json.Marshal(call(map[string]any{"id": a.ID, "op": "list"})["structuredContent"])
	if !strings.Contains(string(listed), `"body":"Bigger button"`) {
		t.Fatal(string(listed))
	}
	reply := call(map[string]any{"id": a.ID, "op": "add", "parent": root.ID, "body": "Done"})["structuredContent"].(map[string]any)
	if reply["author"] != "agent" || reply["parent"] != root.ID {
		t.Fatal(reply)
	}
	call(map[string]any{"id": a.ID, "op": "resolve", "comment_id": root.ID})
	if listed, _ := json.Marshal(call(map[string]any{"id": a.ID, "op": "list"})["structuredContent"]); string(listed) != `{"threads":[]}` {
		t.Fatal(string(listed))
	}
	all, _ := json.Marshal(call(map[string]any{"id": a.ID, "op": "list", "all": true})["structuredContent"])
	if !strings.Contains(string(all), `"replies":[{`) || !strings.Contains(string(all), `"resolved":true`) {
		t.Fatal(string(all))
	}
	if w = request(h, "PUT", "/api/artifacts/"+a.ID+"/comments/"+root.ID, `{"resolved":false}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w = request(h, "GET", "/api/artifacts", ""); !strings.Contains(w.Body.String(), `"open_comments":1`) {
		t.Fatal(w.Body.String())
	}
}

func TestMCPFiles(t *testing.T) {
	h := testServer(t)
	call := func(name string, args map[string]any) map[string]any {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
		return mcpRequest(t, h, "http://127.0.0.1:8080", string(b))
	}
	created := call("create_artifact", map[string]any{"title": "App", "kind": "react", "content": "import C from './C'; export default C",
		"files": map[string]string{"C.tsx": "import './c.css'; export default () => <h1>One</h1>", "c.css": "h1{}", "notes.md": "x"}})["structuredContent"].(map[string]any)
	if created["warnings"] != nil {
		t.Fatal(created)
	}
	id := created["id"].(string)
	updated := call("update_artifact", map[string]any{"id": id, "base_version": 1, "files": map[string]any{"notes.md": nil}, "edits": []map[string]string{{"file": "C.tsx", "old_str": "One", "new_str": "Two"}}})
	if updated["isError"] == true {
		t.Fatal(updated)
	}
	rev := call("get_artifact", map[string]any{"id": id})["structuredContent"].(map[string]any)["revisions"].([]any)[0].(map[string]any)
	files := rev["files"].(map[string]any)
	if len(files) != 2 || !strings.Contains(files["C.tsx"].(string), "Two") || rev["content"] != "import C from './C'; export default C" {
		t.Fatal(rev)
	}
	// Removing a file the page imports is caught by the save's checks.
	broken := call("update_artifact", map[string]any{"id": id, "base_version": 2, "files": map[string]any{"C.tsx": nil}})["structuredContent"].(map[string]any)
	if w, _ := broken["warnings"].([]any); len(w) != 1 || !strings.Contains(w[0].(string), "not among the artifact's files") {
		t.Fatal(broken)
	}
}
