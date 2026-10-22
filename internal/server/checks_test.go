package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCheckArtifact(t *testing.T) {
	for _, tc := range []struct {
		kind, content string
		want          []string
	}{
		{"react", `import x from 'left-pad'; export default x`, []string{`React does not compile: package "left-pad" is unavailable`}},
		{"react", `export default () => <h1>ok</h1>`, nil},
		{"html", `<script src="https://cdnjs.cloudflare.com/ajax/libs/d3/7.8.5/d3.min.js"></script><link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Inter"><link rel="canonical" href="https://example.com/"><img src="https://example.com/a.png"><img src="/media/x.png"><a href="https://example.com/">ok</a>`,
			[]string{"blocked by the sandbox: https://fonts.googleapis.com/css2?family=Inter", "blocked by the sandbox: https://example.com/a.png"}},
		{"html", `<script>fetch('https://api.example.com/data')</script>`, []string{"network requests are blocked: https://api.example.com/data"}},
		{"markdown", "![x](https://example.com/cat.png) [link](https://example.com)", []string{"blocked by the sandbox: https://example.com/cat.png"}},
		{"code", `<img src="https://example.com/a.png">`, nil},
	} {
		got := checkArtifact(tc.kind, tc.content, nil)
		if len(got) != len(tc.want) {
			t.Fatalf("%s %q: %v", tc.kind, tc.content, got)
		}
		for _, want := range tc.want {
			if !strings.Contains(strings.Join(got, "\n"), want) {
				t.Fatalf("%s: got %q, want %q", tc.kind, got, want)
			}
		}
	}
}

func TestRuntimeErrorsReachAgents(t *testing.T) {
	h := testServer(t)
	call := func(name string, args map[string]any) map[string]any {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
		return mcpRequest(t, h, "http://127.0.0.1:8080", string(b))["structuredContent"].(map[string]any)
	}
	created := call("create_artifact", map[string]any{"title": "Broken", "kind": "react", "content": "import x from 'nope'; export default x"})
	if warnings, _ := created["warnings"].([]any); len(warnings) != 1 || !strings.Contains(warnings[0].(string), "does not compile") {
		t.Fatal(created)
	}
	id := created["id"].(string)
	for i := 0; i < 2; i++ {
		if w := request(h, "POST", "/api/artifacts/"+id+"/errors", `{"version":1,"message":"TypeError: x is undefined (line 3)"}`); w.Code != 202 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	for _, bad := range []string{`{"version":2,"message":"x"}`, `{"version":1,"message":" "}`} {
		if w := request(h, "POST", "/api/artifacts/"+id+"/errors", bad); w.Code != 400 {
			t.Fatal(bad, w.Code)
		}
	}
	got := call("get_artifact", map[string]any{"id": id})
	errs, _ := got["runtime_errors"].([]any)
	if len(errs) != 1 || errs[0].(map[string]any)["count"] != float64(2) {
		t.Fatal(got)
	}
	// A new version starts clean.
	call("update_artifact", map[string]any{"id": id, "base_version": 1, "content": "export default () => <h1>fixed</h1>"})
	if got = call("get_artifact", map[string]any{"id": id}); got["runtime_errors"] != nil {
		t.Fatal(got)
	}
}
