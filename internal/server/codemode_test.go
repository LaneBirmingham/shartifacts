package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/LaneBirmingham/shartifacts/internal/store"
)

// testServerWeb is enough of the web UI for the operations that read it.
func testServerWeb() fstest.MapFS {
	return fstest.MapFS{"index.html": {Data: []byte("hello")}, "hub.html": {Data: []byte("apps")}, "design-system.css": {Data: []byte(":root{}")}}
}

// codeCall posts one JSON-RPC request to /mcp (with query, such as "?mode=tools") and
// returns its result.
func codeCall(t *testing.T, h http.Handler, query, body string) map[string]any {
	t.Helper()
	r := httptest.NewRequest("POST", "http://127.0.0.1:8080/mcp"+query, strings.NewReader(body))
	r.RemoteAddr = "192.168.1.9:1234"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("MCP %d: %s", w.Code, w.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["error"] != nil {
		t.Fatal(out)
	}
	return out["result"].(map[string]any)
}

func toolNames(t *testing.T, h http.Handler, query string) []string {
	t.Helper()
	names := []string{}
	for _, tool := range codeCall(t, h, query, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)["tools"].([]any) {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	return names
}

// runCode calls search or execute with code and returns the text it answered, the console
// text (if any) and whether it was an error.
func runCode(t *testing.T, h http.Handler, tool, code string) (string, string, bool) {
	t.Helper()
	args, _ := json.Marshal(map[string]any{"name": tool, "arguments": map[string]string{"code": code}})
	res := codeCall(t, h, "", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":`+string(args)+`}`)
	content := res["content"].([]any)
	text, logs := content[0].(map[string]any)["text"].(string), ""
	if len(content) > 1 {
		logs = content[1].(map[string]any)["text"].(string)
	}
	isErr, _ := res["isError"].(bool)
	return text, logs, isErr
}

func TestCodeModeIsTheDefault(t *testing.T) {
	h := testServer(t)
	if got := toolNames(t, h, ""); strings.Join(got, ",") != "execute,search" {
		t.Fatalf("code mode should offer search and execute: %v", got)
	}
	init := codeCall(t, h, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	instructions := init["instructions"].(string)
	for _, part := range []string{"execute runs an async JavaScript arrow function", "shartifacts.list_artifacts", "spec.guide in search", "get_artifact for the latest base_version", "list_media", "get_design_system", "/api/media"} {
		if !strings.Contains(instructions, part) {
			t.Fatalf("instructions miss %q:\n%s", part, instructions)
		}
	}
	tools := toolNames(t, h, "?mode=tools")
	if len(tools) != 18 || !strings.Contains(strings.Join(tools, ","), "list_artifacts") {
		t.Fatalf("mode=tools should offer every operation as a tool: %v", tools)
	}
	init = codeCall(t, h, "?mode=tools", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	if strings.Contains(init["instructions"].(string), "execute") {
		t.Fatal("tools mode should not mention execute")
	}
	// An unknown mode is refused, saying which there are.
	r := httptest.NewRequest("POST", "http://127.0.0.1:8080/mcp?mode=classic", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	r.RemoteAddr = "192.168.1.9:1234"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "mode=tools") {
		t.Fatalf("unknown mode: %d %s", w.Code, w.Body)
	}

	// A server can default to tools, and a connection can still ask for code.
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tools2 := (&Server{Store: st, Web: testServerWeb(), MCPMode: "tools"}).Handler()
	if got := toolNames(t, tools2, ""); len(got) != 18 {
		t.Fatalf("MCPMode tools: %v", got)
	}
	if got := toolNames(t, tools2, "?mode=code"); strings.Join(got, ",") != "execute,search" {
		t.Fatalf("mode=code on a tools server: %v", got)
	}
}

func TestCodeModeSearch(t *testing.T) {
	h := testServer(t)
	text, _, isErr := runCode(t, h, "search", `async () => Object.keys(spec.operations)`)
	var names []string
	if isErr || json.Unmarshal([]byte(text), &names) != nil || len(names) != 18 || names[0] != "list_artifacts" || names[4] != "update_artifact" {
		t.Fatalf("the spec should list every operation in order: %s", text)
	}
	text, _, _ = runCode(t, h, "search", `async () => spec.operations.create_artifact.signature`)
	for _, part := range []string{"create_artifact(input: {", "  title: string;", "  kind: string;", "  tags?: string[];", "/** Also make a public read-only link", "}): Promise<any>"} {
		if !strings.Contains(text, part) {
			t.Fatalf("signature misses %q:\n%s", part, text)
		}
	}
	text, _, _ = runCode(t, h, "search", `async () => ({input: spec.operations.update_artifact.input.properties.base_version, destructive: Object.entries(spec.operations).filter(([, op]) => op.destructive).map(([name]) => name)})`)
	if !strings.Contains(text, `"type":"integer"`) || !strings.Contains(text, `"destructive":["delete_artifact","manage_app"]`) {
		t.Fatalf("schemas and destructive flags: %s", text)
	}
	text, _, _ = runCode(t, h, "search", `async () => spec.guide.filter(p => p.startsWith("Ports:")).length`)
	if text != "1" {
		t.Fatalf("the guide should be searchable by paragraph: %s", text)
	}
	// search has no operations to call.
	if text, _, isErr := runCode(t, h, "search", `async () => shartifacts.list_artifacts()`); !isErr || !strings.Contains(text, "shartifacts is not defined") {
		t.Fatalf("search reached the operations: %s", text)
	}
}

func TestCodeModeExecute(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := (&Server{Store: st, Web: testServerWeb()}).Handler()
	// One run creates, edits and reads back an artifact.
	text, logs, isErr := runCode(t, h, "execute", `async () => {
		const made = await shartifacts.create_artifact({title: "Packing list", kind: "html", content: "<h1>Pack</h1><p>socks</p>", tags: ["travel"]})
		console.log("made", made.id, "v" + made.version)
		const edited = await shartifacts.update_artifact({id: made.id, base_version: made.version, edits: [{old_str: "socks", new_str: "socks, hat"}]})
		const a = await shartifacts.get_artifact({id: made.id})
		return {id: made.id, version: edited.version, content: a.revisions[0].content, tags: a.tags}
	}`)
	var out struct {
		ID      string   `json:"id"`
		Version int      `json:"version"`
		Content string   `json:"content"`
		Tags    []string `json:"tags"`
	}
	if isErr || json.Unmarshal([]byte(text), &out) != nil || out.Version != 2 || out.Content != "<h1>Pack</h1><p>socks, hat</p>" || len(out.Tags) != 1 {
		t.Fatalf("%s", text)
	}
	if logs != "console:\nmade "+out.ID+" v1" {
		t.Fatalf("console output: %q", logs)
	}
	// A failing operation throws with the tool's own message, and the code can catch it.
	text, _, isErr = runCode(t, h, "execute", fmt.Sprintf(`async () => {
		try { await shartifacts.update_artifact({id: %q, base_version: 1, content: "stale"}) } catch (e) { return e.message }
	}`, out.ID))
	if isErr || !strings.Contains(text, "reload the latest version") {
		t.Fatalf("a stale base_version should fail as the tool would: %s", text)
	}
	// Uncaught, the run fails and says what it had already called.
	text, _, isErr = runCode(t, h, "execute", `async () => {
		await shartifacts.list_artifacts({limit: 1})
		await shartifacts.get_artifact({id: "nope"})
	}`)
	if !isErr || !strings.Contains(text, "Operations it called before stopping: list_artifacts, get_artifact (failed:") {
		t.Fatalf("an uncaught failure: %s", text)
	}
	// Input is validated as the tool validates it.
	if text, _, isErr := runCode(t, h, "execute", `async () => shartifacts.get_artifact({id: 7})`); !isErr || !strings.Contains(text, "validating") {
		t.Fatalf("a mistyped argument: %s", text)
	}
	// A tool that answers in text resolves to a string, returned as itself.
	if text, _, isErr := runCode(t, h, "execute", `async () => (await shartifacts.get_design_system()).slice(0, 25)`); isErr || text != "shartifacts design system" {
		t.Fatalf("text result: %q", text)
	}
	if text, _, _ := runCode(t, h, "execute", `async () => { await shartifacts.list_artifacts() }`); !strings.HasPrefix(text, "undefined") {
		t.Fatalf("no return value: %s", text)
	}
}

// A deletion's two calls are for the person to agree in between, so code cannot confirm
// with the token its own run was handed; a later run, after they agree, can.
func TestCodeModeConfirmNeedsANewRun(t *testing.T) {
	h := testServer(t)
	text, _, isErr := runCode(t, h, "execute", `async () => {
		const a = await shartifacts.create_artifact({title: "Doomed", kind: "markdown", content: "# bye"})
		const ask = await shartifacts.delete_artifact({id: a.id, base_version: 1})
		let refused = ""
		try { await shartifacts.delete_artifact({id: a.id, base_version: 1, confirm: ask.confirm}) } catch (e) { refused = e.message }
		return {id: a.id, confirm: ask.confirm, refused}
	}`)
	var out struct{ ID, Confirm, Refused string }
	if isErr || json.Unmarshal([]byte(text), &out) != nil || out.Confirm == "" || !strings.Contains(out.Refused, "ask them") {
		t.Fatalf("%s", text)
	}
	text, _, isErr = runCode(t, h, "execute", fmt.Sprintf(`async () => shartifacts.delete_artifact({id: %q, base_version: 1, confirm: %q})`, out.ID, out.Confirm))
	if isErr || !strings.Contains(text, `"deleted":true`) {
		t.Fatalf("confirming in a new run: %s", text)
	}
}
