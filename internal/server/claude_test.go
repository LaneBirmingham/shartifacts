package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LaneBirmingham/shartifacts/internal/store"
	"github.com/anthropics/anthropic-sdk-go/option"
)

func TestClaudeComplete(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	off := (&Server{Store: st}).Handler()
	if w := request(off, "POST", "/api/complete", `{"prompt":"hi"}`); w.Code != 503 || !strings.Contains(w.Body.String(), "ANTHROPIC_API_KEY") {
		t.Fatal(w.Code, w.Body.String())
	}
	var got map[string]any
	var beta string
	stopReason := "end_turn"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = map[string]any{}
		json.Unmarshal(b, &got)
		beta = r.Header.Get("Anthropic-Beta")
		if r.Header.Get("X-Api-Key") != "test-key" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"Four."}],"stop_reason":"`+stopReason+`","usage":{"input_tokens":3,"output_tokens":2}}`)
	}))
	defer upstream.Close()
	h := (&Server{Store: st, Claude: NewClaude("test-key", "claude-opus-5-5", option.WithBaseURL(upstream.URL), option.WithMaxRetries(0))}).Handler()
	w := request(h, "POST", "/api/complete", `{"prompt":"What is 2+2?"}`)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"text":"Four."}` {
		t.Fatal(w.Code, w.Body.String())
	}
	if got["model"] != "claude-opus-5-5" || got["fallbacks"] != "default" || !strings.Contains(beta, "server-side-fallback-2026-07-01") {
		t.Fatal(got, beta)
	}
	w = request(h, "POST", "/api/complete", `{"messages":[{"role":"user","content":"Hi"},{"role":"assistant","content":"Hello"},{"role":"user","content":"Again"}]}`)
	if w.Code != 200 || len(got["messages"].([]any)) != 3 {
		t.Fatal(w.Code, got)
	}
	for _, bad := range []string{`{}`, `{"prompt":"a","messages":[]}`, `{"messages":[{"role":"system","content":"x"}]}`} {
		if w = request(h, "POST", "/api/complete", bad); w.Code != 400 {
			t.Fatalf("%s: %d", bad, w.Code)
		}
	}
	stopReason = "refusal"
	if w = request(h, "POST", "/api/complete", `{"prompt":"x"}`); w.Code != 502 || !strings.Contains(w.Body.String(), "declined") {
		t.Fatal(w.Code, w.Body.String())
	}
	// Models without the server-side fallback get none.
	haiku := (&Server{Store: st, Claude: NewClaude("test-key", "claude-haiku-5-5", option.WithBaseURL(upstream.URL), option.WithMaxRetries(0))}).Handler()
	stopReason = "end_turn"
	request(haiku, "POST", "/api/complete", `{"prompt":"x"}`)
	if _, ok := got["fallbacks"]; ok {
		t.Fatal("fallbacks sent for a model without them")
	}
	bad := (&Server{Store: st, Claude: NewClaude("wrong", "claude-opus-5-5", option.WithBaseURL(upstream.URL), option.WithMaxRetries(0))}).Handler()
	if w = request(bad, "POST", "/api/complete", `{"prompt":"x"}`); w.Code != 502 || !strings.Contains(w.Body.String(), "API key was rejected") {
		t.Fatal(w.Code, w.Body.String())
	}
}
