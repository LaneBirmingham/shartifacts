package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPortRegistryOverHTTP(t *testing.T) {
	h := testServer(t)
	if w := call(h, "POST", "/api/ports", `{"host":"MyServer","port":8123,"service":"grafana","owner":"Codex","note":"dashboards"}`); w.Code != 201 {
		t.Fatalf("claim: %d %s", w.Code, w.Body)
	}
	// The same service again is an update; another is refused with the holder and a way out.
	if w := call(h, "POST", "/api/ports", `{"host":"myserver","port":8123,"service":"grafana"}`); w.Code != 200 || !strings.Contains(w.Body.String(), "dashboards") {
		t.Fatalf("claim again: %d %s", w.Code, w.Body)
	}
	w := call(h, "POST", "/api/ports", `{"host":"myserver","port":8123,"service":"prometheus"}`)
	taken := decodeBody(t, w)
	if w.Code != 409 || taken["code"] != "port_taken" || taken["next_free"] != float64(8124) || taken["holder"].(map[string]any)["service"] != "grafana" || !strings.Contains(taken["error"].(string), "grafana") {
		t.Fatalf("taken: %d %s", w.Code, w.Body)
	}
	w = call(h, "POST", "/api/ports", `{"host":"myserver","from":8123,"service":"prometheus"}`)
	if w.Code != 201 || decodeBody(t, w)["port"] != float64(8124) {
		t.Fatalf("next free: %d %s", w.Code, w.Body)
	}
	for _, body := range []string{`{"host":"my server","port":1,"service":"x"}`, `{"host":"myserver","port":1}`, `{"host":"myserver","port":1,"service":"x","extra":1}`} {
		if w := call(h, "POST", "/api/ports", body); w.Code != 400 {
			t.Errorf("%s: %d", body, w.Code)
		}
	}
	call(h, "POST", "/api/ports", `{"host":"192.168.1.20","port":53,"protocol":"udp","service":"pihole"}`)

	var listed struct {
		Claims []struct {
			Host, Protocol, Service string
			Port                    int
		}
		Hosts []string
	}
	json.Unmarshal(call(h, "GET", "/api/ports?host=myserver", "").Body.Bytes(), &listed)
	if len(listed.Claims) != 2 || listed.Claims[1].Service != "prometheus" || strings.Join(listed.Hosts, ",") != "192.168.1.20,myserver" {
		t.Fatalf("list: %+v", listed)
	}
	if w := call(h, "DELETE", "/api/ports/192.168.1.20/53", ""); w.Code != 200 || decodeBody(t, w)["released"] != false {
		t.Fatalf("release on the wrong protocol: %d %s", w.Code, w.Body)
	}
	if w := call(h, "DELETE", "/api/ports/192.168.1.20/53?protocol=udp", ""); w.Code != 200 || decodeBody(t, w)["released"] != true {
		t.Fatalf("release: %d %s", w.Code, w.Body)
	}
	if w := call(h, "DELETE", "/api/ports/myserver/http", ""); w.Code != 400 {
		t.Fatalf("release a port that is no number: %d", w.Code)
	}
}

func TestPortsTool(t *testing.T) {
	h := testServer(t)
	tool := func(args string) map[string]any {
		t.Helper()
		return mcpRequest(t, h, "http://127.0.0.1:8080", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ports","arguments":`+args+`}}`)
	}
	// Agents learn to claim before they publish a port when they connect, not only when
	// they happen to read the tool list.
	init := mcpRequest(t, h, "http://127.0.0.1:8080", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	if !strings.Contains(init["instructions"].(string), "claim it with ports") {
		t.Fatal("the instructions do not mention the port registry")
	}
	if r := tool(`{"op":"claim","host":"myserver","from":8100,"service":"uptime-kuma"}`); r["isError"] == true || r["structuredContent"].(map[string]any)["claim"].(map[string]any)["port"] != float64(8100) {
		t.Fatal(r)
	}
	r := tool(`{"op":"claim","host":"myserver","port":8100,"service":"grafana"}`)
	if text, _ := json.Marshal(r["content"]); r["isError"] != true || !strings.Contains(string(text), "next free port is 8101") {
		t.Fatalf("taken port over MCP: %v", r)
	}
	if r := tool(`{"host":"myserver"}`)["structuredContent"].(map[string]any); len(r["claims"].([]any)) != 1 || r["hosts"].([]any)[0] != "myserver" {
		t.Fatal(r)
	}
	if r := tool(`{"op":"release","host":"myserver","port":8100}`)["structuredContent"].(map[string]any); r["released"] != true {
		t.Fatal(r)
	}
	if r := tool(`{"op":"release","host":"myserver"}`); r["isError"] != true {
		t.Fatal("release without a port", r)
	}
}

// The registry is part of the workspace: a signed-in agent uses it, and is named as the
// owner of what it claims; without sign-in, cross-site or on the share hostname it is refused.
func TestPortRegistryAccess(t *testing.T) {
	_, h := signInServer(t)
	owner := cookieOf(t, call(h, "PUT", "/api/auth/password", `{"password":"correct horse"}`))
	bearer := "Bearer " + decodeBody(t, call(h, "POST", "/api/auth/tokens", `{"name":"Codex on my laptop"}`, "Cookie", owner))["token"].(string)

	claim := `{"host":"myserver","port":8123,"service":"grafana"}`
	w := call(h, "POST", "/api/ports", claim, "Authorization", bearer)
	if w.Code != 201 || decodeBody(t, w)["owner"] != "Codex on my laptop" {
		t.Fatalf("claim with a token: %d %s", w.Code, w.Body)
	}
	if w := call(h, "GET", "/api/ports", "", "Cookie", owner); w.Code != 200 || !strings.Contains(w.Body.String(), "grafana") {
		t.Fatalf("list in a signed-in browser: %d %s", w.Code, w.Body)
	}
	mcp := call(h, "POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ports","arguments":{"op":"claim","host":"myserver","from":9000,"service":"loki"}}}`, "Accept", "application/json, text/event-stream", "Authorization", bearer)
	if mcp.Code != 200 || !strings.Contains(mcp.Body.String(), `"owner":"Codex on my laptop"`) {
		t.Fatalf("claim over MCP with a token: %d %s", mcp.Code, mcp.Body)
	}

	for _, tc := range []struct {
		name, method, target string
		headers              []string
		want                 int
	}{
		{"no sign-in", "GET", "/api/ports", nil, 401},
		{"no sign-in claim", "POST", "/api/ports", nil, 401},
		{"no sign-in release", "DELETE", "/api/ports/myserver/8123", nil, 401},
		{"wrong token", "GET", "/api/ports", []string{"Authorization", "Bearer shx_wrong"}, 401},
		{"cross-site claim", "POST", "/api/ports", []string{"Cookie", owner, "Sec-Fetch-Site", "cross-site", "Sec-Fetch-Mode", "cors"}, 403},
		{"share hostname", "GET", "https://share.example.com/api/ports", []string{"Authorization", bearer}, 404},
		{"share hostname claim", "POST", "https://share.example.com/api/ports", []string{"Authorization", bearer}, 404},
	} {
		body := ""
		if tc.method == "POST" {
			body = `{"host":"myserver","port":8124,"service":"intruder"}`
		}
		if w := call(h, tc.method, tc.target, body, tc.headers...); w.Code != tc.want {
			t.Errorf("%s: %d, want %d (%s)", tc.name, w.Code, tc.want, w.Body)
		}
	}
	if w := call(h, "GET", "/api/ports", "", "Authorization", bearer); strings.Contains(w.Body.String(), "intruder") || !strings.Contains(w.Body.String(), "grafana") {
		t.Fatalf("a refused request changed the registry: %s", w.Body)
	}
}
