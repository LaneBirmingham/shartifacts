package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tool calls an MCP tool and returns its structured result ({"text"} for a text one), or its
// error text.
func tool(t *testing.T, h http.Handler, name string, args map[string]any) (map[string]any, string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	res := mcpRequest(t, h, "http://127.0.0.1:8080", string(b))
	if res["isError"] == true {
		return nil, res["content"].([]any)[0].(map[string]any)["text"].(string)
	}
	if sc, ok := res["structuredContent"].(map[string]any); ok {
		return sc, ""
	}
	return map[string]any{"text": res["content"].([]any)[0].(map[string]any)["text"]}, ""
}

func TestListToolsPage(t *testing.T) {
	h := testServer(t)
	for i := 0; i < 5; i++ {
		if w := request(h, "POST", "/api/artifacts", fmt.Sprintf(`{"title":"a%d","kind":"code","content":"x"}`, i)); w.Code != 201 {
			t.Fatal(w.Body)
		}
	}
	all, _ := tool(t, h, "list_artifacts", nil)
	if len(all["artifacts"].([]any)) != 5 || all["total"] != float64(5) || all["next_cursor"] != nil {
		t.Fatalf("one page: %v", all)
	}
	var titles []string
	cursor := ""
	for pages := 0; ; pages++ {
		args := map[string]any{"limit": 2}
		if cursor != "" {
			args["cursor"] = cursor
		}
		page, errText := tool(t, h, "list_artifacts", args)
		if errText != "" || pages > 3 {
			t.Fatal(errText, pages)
		}
		for _, a := range page["artifacts"].([]any) {
			titles = append(titles, a.(map[string]any)["title"].(string))
		}
		next, _ := page["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	// Newest first, each once.
	if strings.Join(titles, ",") != "a4,a3,a2,a1,a0" {
		t.Fatal(titles)
	}
	if _, errText := tool(t, h, "list_artifacts", map[string]any{"cursor": "not-a-cursor"}); !strings.Contains(errText, "cursor") {
		t.Fatalf("bad cursor: %q", errText)
	}

	for _, f := range []struct{ name, mime, body string }{{"sales.csv", "text/csv", "a,b"}, {"Logo.png", "image/png", "png"}, {"photo.jpg", "image/jpeg", "jpg"}} {
		if w := call(h, "POST", "/api/media?name="+f.name, f.body, "Content-Type", f.mime); w.Code != 201 {
			t.Fatal(w.Body)
		}
	}
	names := func(args map[string]any) string {
		t.Helper()
		res, errText := tool(t, h, "list_media", args)
		if errText != "" {
			t.Fatal(errText)
		}
		var out []string
		for _, m := range res["media"].([]any) {
			out = append(out, m.(map[string]any)["name"].(string))
		}
		if res["upload_url"] == nil {
			t.Fatal("list_media lost the upload URL")
		}
		return fmt.Sprintf("%s/%v", strings.Join(out, ","), res["total"])
	}
	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{nil, "photo.jpg,Logo.png,sales.csv/3"},
		{map[string]any{"name": "LOGO"}, "Logo.png/1"},
		{map[string]any{"type": "image"}, "photo.jpg,Logo.png/2"},
		{map[string]any{"type": "image/png"}, "Logo.png/1"},
		{map[string]any{"type": "text/"}, "sales.csv/1"},
		{map[string]any{"type": "video"}, "/0"},
		{map[string]any{"limit": 1}, "photo.jpg/3"},
	} {
		if got := names(c.args); got != c.want {
			t.Errorf("%v: %s, want %s", c.args, got, c.want)
		}
	}
}

func TestMediaDeleteAndUsage(t *testing.T) {
	s, h := appServer(t, nil)
	upload := func(name, body string) string {
		t.Helper()
		w := call(h, "POST", "/api/media?name="+name, body, "Content-Type", "image/png")
		if w.Code != 201 {
			t.Fatal(w.Body)
		}
		return decodeBody(t, w)["id"].(string)
	}
	used, unused := upload("used.png", "one"), upload("unused.png", "two")
	if w := call(h, "POST", "/api/artifacts", `{"title":"Gallery","kind":"html","content":"<img src=\"/media/`+used+`\">"}`); w.Code != 201 {
		t.Fatal(w.Body)
	}
	plain := decodeBody(t, call(h, "GET", "/api/media", ""))["media"].([]any)
	if _, ok := plain[0].(map[string]any)["used_by"]; ok {
		t.Fatal("usage scanned without being asked for")
	}
	usage := map[string]string{}
	for _, m := range decodeBody(t, call(h, "GET", "/api/media?usage=1", ""))["media"].([]any) {
		item := m.(map[string]any)
		var by []string
		for _, u := range item["used_by"].([]any) {
			by = append(by, u.(map[string]any)["title"].(string))
		}
		usage[item["id"].(string)] = strings.Join(by, ",")
	}
	if usage[used] != "Gallery" || usage[unused] != "" || len(usage) != 2 {
		t.Fatalf("usage %v", usage)
	}

	// Refused: a sandboxed page (Origin null), another site, and the public share hostname.
	for _, c := range []struct {
		name, target string
		headers      []string
	}{
		{"sandboxed page", "/api/media/" + unused, []string{"Origin", "null"}},
		{"cross-site", "/api/media/" + unused, []string{"Sec-Fetch-Site", "cross-site", "Sec-Fetch-Mode", "cors"}},
		{"share host", "https://share.example.com/api/media/" + unused, nil},
	} {
		if w := call(h, "DELETE", c.target, "", c.headers...); w.Code == 200 {
			t.Fatalf("%s deleted media", c.name)
		}
	}
	if _, err := os.Stat(filepath.Join(s.Store.Dir(), "media", unused)); err != nil {
		t.Fatalf("a refused delete removed the file: %v", err)
	}
	// Allowed: the workspace.
	if w := call(h, "DELETE", "/api/media/"+unused, ""); w.Code != 200 {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if w := call(h, "GET", "/media/"+unused, ""); w.Code != 404 {
		t.Fatalf("deleted media served: %d", w.Code)
	}
	for _, id := range []string{unused, "nope.png", "..%2fmeta%2fshares.json"} {
		if w := call(h, "DELETE", "/api/media/"+id, ""); w.Code == 200 {
			t.Fatalf("%s: %d", id, w.Code)
		}
	}
	if w := call(h, "GET", "/media/"+used, ""); w.Code != 200 {
		t.Fatalf("the other file: %d", w.Code)
	}
}

func TestAppVersionsViewAndUsage(t *testing.T) {
	_, h := appServer(t, nil)
	w := call(h, "POST", "/api/apps", `{"name":"Site","files":{"index.html":"<h1>v1</h1>"}}`)
	if w.Code != 201 {
		t.Fatal(w.Body)
	}
	id := decodeBody(t, w)["app"].(map[string]any)["id"].(string)
	for v := 2; v <= 13; v++ {
		body := fmt.Sprintf(`{"base_version":%d,"merge":true,"edits":[{"file":"index.html","old_str":"v%d","new_str":"v%d"}]}`, v-1, v-1, v)
		if w := call(h, "POST", "/api/apps/"+id+"/deployments", body); w.Code != 201 {
			t.Fatalf("v%d: %s", v, w.Body)
		}
	}
	call(h, "PUT", "/api/apps/"+id+"/live", `{"version":1}`)
	if w := call(h, "POST", "/api/apps/"+id+"/shares", `{"version":2}`); w.Code != 201 {
		t.Fatal(w.Body)
	}
	versions := func(view map[string]any) []string {
		var out []string
		for _, d := range view["deployments"].([]any) {
			out = append(out, fmt.Sprint(d.(map[string]any)["version"]))
		}
		return out
	}
	got, _ := tool(t, h, "get_app", map[string]any{"id": id})
	// The newest ten, then the linked and live versions; version 3 is left out.
	if v := strings.Join(versions(got), ","); v != "13,12,11,10,9,8,7,6,5,4,2,1" || got["older_versions_omitted"] != float64(1) {
		t.Fatalf("versions %s, omitted %v", v, got["older_versions_omitted"])
	}
	if got["keep_versions"] != float64(20) || got["oldest_rollback"] != float64(1) || got["latest"] != float64(13) {
		t.Fatalf("%v", got)
	}
	if all, _ := tool(t, h, "get_app", map[string]any{"id": id, "all_versions": true}); len(versions(all)) != 13 || all["older_versions_omitted"] != nil {
		t.Fatalf("all versions: %v", versions(all))
	}
	// The hub reads every version.
	if hub := decodeBody(t, call(h, "GET", "/api/apps/"+id, "")); len(versions(hub)) != 13 || hub["oldest_rollback"] != float64(1) {
		t.Fatalf("hub: %v", versions(hub))
	}
	if guide, _ := tool(t, h, "get_app", map[string]any{"guide": true}); !strings.Contains(guide["text"].(string), "Rollback reaches the newest 20 versions that built") {
		t.Fatal("the guide does not state the rollback window")
	}

	u := decodeBody(t, call(h, "GET", "/api/apps/"+id+"/usage", ""))
	if u["versions"] != float64(13) || u["versions_bytes"].(float64) <= 0 || u["keep_versions"] != float64(20) || u["data_quota"] != float64(0) {
		t.Fatalf("usage %v", u)
	}
	if w := call(h, "GET", "/api/apps/000000000000000000000000/usage", ""); w.Code != 404 {
		t.Fatalf("unknown app: %d", w.Code)
	}
}

func TestUnchangedGoBackendReusesItsBuild(t *testing.T) {
	s, h := appServer(t, nil)
	w := call(h, "POST", "/api/apps", `{"name":"Notes","template":"notes"}`)
	if w.Code != 201 {
		t.Fatal(w.Body)
	}
	id := decodeBody(t, w)["app"].(map[string]any)["id"].(string)
	deploy := func(base int, change string) map[string]any {
		t.Helper()
		w := call(h, "POST", "/api/apps/"+id+"/deployments", fmt.Sprintf(`{"base_version":%d,"merge":true,%s}`, base, change))
		if w.Code != 201 {
			t.Fatalf("deploy: %d %s", w.Code, w.Body)
		}
		d := decodeBody(t, w)["deployment"].(map[string]any)
		if d["status"] != "ready" {
			t.Fatalf("v%d: %v", base+1, d)
		}
		return d
	}
	server := func(v int) os.FileInfo {
		info, err := os.Stat(filepath.Join(s.Store.AppsDir(), id, "v", fmt.Sprint(v), "out", "server"))
		if err != nil {
			t.Fatal(err)
		}
		return info
	}
	// A frontend change: the backend is the same build.
	d2 := deploy(1, `"edits":[{"file":"src/App.tsx","old_str":"<h1>Notes{","new_str":"<h1>My notes{"}]`)
	if !strings.Contains(fmt.Sprint(d2["log"]), "reused that build") || !os.SameFile(server(1), server(2)) {
		t.Fatalf("v2 log: %v", d2["log"])
	}
	// A backend change builds again, and its version's binary is its own.
	d3 := deploy(2, `"edits":[{"file":"server/main.go","old_str":"\"ok\": true,","new_str":"\"ok\": true, \"v\": 3,"}]`)
	if strings.Contains(fmt.Sprint(d3["log"]), "reused") || os.SameFile(server(2), server(3)) {
		t.Fatalf("v3 log: %v", d3["log"])
	}
	if health := decodeBody(t, call(h, "GET", "/run/"+id+"/live/api/health", "")); health["v"] != float64(3) {
		t.Fatalf("v3 health: %v", health)
	}
	if health := decodeBody(t, call(h, "GET", "/run/"+id+"/2/api/health", "")); health["version"] != "2" || health["v"] != nil {
		t.Fatalf("v2 health: %v", health)
	}
}
