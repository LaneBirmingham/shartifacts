package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
)

// The `sample` capability: one round of a conversation a page holds with Claude. The
// page's bridge (web/bridge.js) keeps the turns, runs the page's tools between rounds and
// sends each round here in the Messages API's own shape, so tool calls and thinking blocks
// come back exactly as Claude wrote them. Only text, base64 images, tool calls and results
// and thinking are accepted, and only the page's own tools: nothing that runs server-side.

const sampleSystem = "You are Claude, answering a request from an interactive page (an artifact) on behalf of the person viewing it. The page sends you only what is in this conversation: you remember nothing between requests and have no tools except any the page offers. Earlier assistant turns are what the page says your earlier replies were."
const sampleJSON = " The page's code will parse your final message as one JSON value: reply with exactly the JSON it asks for and nothing else."

var tierModels = map[string]string{"quick": "claude-haiku-5-5", "complex": "claude-opus-5-5"}

type sampleRequest struct {
	Messages  []json.RawMessage `json:"messages"`
	Tools     []json.RawMessage `json:"tools,omitempty"`
	Model     string            `json:"model,omitempty"`
	ModelTier string            `json:"model_tier,omitempty"`
	Effort    string            `json:"effort,omitempty"`
	JSON      bool              `json:"json,omitempty"`
	Stream    bool              `json:"stream,omitempty"`
}

type sampleError struct{ code, message string }

func (e sampleError) Error() string { return e.message }

var allowedBlocks = map[string]bool{"text": true, "image": true, "tool_use": true, "tool_result": true, "thinking": true, "redacted_thinking": true}

// params validates a round and builds its API request.
func (c *Claude) params(in sampleRequest) (anthropic.BetaMessageNewParams, string, error) {
	bad := func(format string, a ...any) error { return sampleError{"invalid_request", fmt.Sprintf(format, a...)} }
	if len(in.Messages) == 0 {
		return anthropic.BetaMessageNewParams{}, "", bad("input is empty")
	}
	var text int
	messages := make([]anthropic.BetaMessageParam, 0, len(in.Messages))
	for i, raw := range in.Messages {
		var shape struct {
			Role    string `json:"role"`
			Content []struct {
				Type   string `json:"type"`
				Text   string `json:"text"`
				Source *struct {
					Type string `json:"type"`
				} `json:"source"`
			} `json:"content"`
		}
		if err := json.Unmarshal(raw, &shape); err != nil || (shape.Role != "user" && shape.Role != "assistant") || len(shape.Content) == 0 {
			return anthropic.BetaMessageNewParams{}, "", bad("turn %d must be a user or assistant turn with content", i+1)
		}
		for _, b := range shape.Content {
			if !allowedBlocks[b.Type] || (b.Type == "image" && (b.Source == nil || b.Source.Type != "base64")) {
				return anthropic.BetaMessageNewParams{}, "", bad("turn %d holds a %q block, which pages cannot send", i+1, b.Type)
			}
			text += len(b.Text)
		}
		var m anthropic.BetaMessageParam
		if err := json.Unmarshal(raw, &m); err != nil {
			return anthropic.BetaMessageNewParams{}, "", bad("turn %d: %v", i+1, err)
		}
		messages = append(messages, m)
	}
	if text > 256<<10 {
		return anthropic.BetaMessageNewParams{}, "", sampleError{"prompt_too_large", "input is over 256 KiB of text"}
	}
	tools := make([]anthropic.BetaToolUnionParam, 0, len(in.Tools))
	for i, raw := range in.Tools {
		var t anthropic.BetaToolUnionParam
		if err := json.Unmarshal(raw, &t); err != nil || t.OfTool == nil || t.OfTool.Type != "" && t.OfTool.Type != "custom" {
			return anthropic.BetaMessageNewParams{}, "", bad("tool %d must be a page tool {name, description, input_schema}", i+1)
		}
		tools = append(tools, t)
	}
	tier := in.ModelTier
	if tier == "" {
		tier = "default"
	}
	model, ok := tierModels[tier]
	if tier == "default" {
		model, ok = c.model, true
	}
	if !ok {
		return anthropic.BetaMessageNewParams{}, "", bad("modelTier must be quick, default or complex")
	}
	if in.Model != "" {
		model = in.Model
	}
	system := sampleSystem
	if in.JSON {
		system += sampleJSON
	}
	p := anthropic.BetaMessageNewParams{Model: model, MaxTokens: 16000, Messages: messages, Tools: tools, System: []anthropic.BetaTextBlockParam{{Text: system}}}
	switch in.Effort {
	case "low", "medium", "high":
		p.OutputConfig = anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffort(in.Effort)}
	}
	return p, tier, nil
}

func sampleFailure(err error) sampleError {
	var se sampleError
	if errors.As(err, &se) {
		return se
	}
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		log.Printf("sample: %v", apiErr)
		switch apiErr.StatusCode {
		case 401, 403:
			return sampleError{"sampling_disabled", "the server's Anthropic API key was rejected"}
		case 429, 529:
			return sampleError{"rate_limited", "Claude is busy or rate limited; try again later"}
		case 413:
			return sampleError{"prompt_too_large", "the request is too large"}
		case 400:
			return sampleError{"invalid_request", "Claude rejected the request"}
		}
	}
	if errors.Is(err, context.Canceled) {
		return sampleError{"cancelled", "cancelled"}
	}
	return sampleError{"upstream_error", "Claude is unavailable right now"}
}

// sample runs one round: POST /api/sample. With stream, text arrives as server-sent events
// ({type:"text",delta}) before the final {type:"done"} or {type:"error"}.
func (s *Server) sample(w http.ResponseWriter, r *http.Request) {
	var in sampleRequest
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
	if !decode(w, r, &in) {
		return
	}
	if s.Claude == nil {
		writeJSON(w, 503, map[string]string{"code": "sampling_disabled", "error": "Claude is off: start shartifacts with ANTHROPIC_API_KEY set"})
		return
	}
	params, tier, err := s.Claude.params(in)
	if err != nil {
		e := sampleFailure(err)
		writeJSON(w, 400, map[string]string{"code": e.code, "error": e.message})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	select {
	case s.Claude.slots <- struct{}{}:
		defer func() { <-s.Claude.slots }()
	case <-ctx.Done():
		return
	}
	done := func(msg anthropic.BetaMessage) map[string]any {
		out := map[string]any{"type": "done", "message": msg.ToParam(), "stop_reason": msg.StopReason, "model_tier": tier}
		if in.Model != "" && string(msg.Model) == in.Model {
			out["model"] = in.Model
		}
		return out
	}
	if !in.Stream {
		if fallbackModels[string(params.Model)] {
			params.Betas = []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01}
			params.Fallbacks = anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()}
		}
		msg, err := s.Claude.client.Beta.Messages.New(ctx, params)
		if err != nil {
			e := sampleFailure(err)
			writeJSON(w, 502, map[string]string{"code": e.code, "error": e.message})
			return
		}
		writeJSON(w, 200, done(*msg))
		return
	}
	// Streaming has no server-side fallback: text already shown could not be withdrawn
	// cleanly, so a refusal ends the round instead (the page drops its partial text).
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)
	send := func(v any) {
		b, _ := json.Marshal(v)
		rc.SetWriteDeadline(time.Now().Add(time.Minute))
		fmt.Fprintf(w, "data: %s\n\n", b)
		rc.Flush()
	}
	stream := s.Claude.client.Beta.Messages.NewStreaming(ctx, params)
	defer stream.Close()
	var msg anthropic.BetaMessage
	for stream.Next() {
		ev := stream.Current()
		if err := msg.Accumulate(ev); err != nil {
			send(map[string]string{"type": "error", "code": "upstream_error", "message": err.Error()})
			return
		}
		if d, ok := ev.AsAny().(anthropic.BetaRawContentBlockDeltaEvent); ok {
			if t, ok := d.Delta.AsAny().(anthropic.BetaTextDelta); ok && t.Text != "" {
				send(map[string]string{"type": "text", "delta": t.Text})
			}
		}
	}
	if err := stream.Err(); err != nil {
		e := sampleFailure(err)
		send(map[string]string{"type": "error", "code": e.code, "message": e.message})
		return
	}
	send(done(msg))
}

// sampleLimits tells pages what this server can do: GET /api/sample/limits
func (s *Server) sampleLimits(w http.ResponseWriter, r *http.Request) {
	if s.Claude == nil {
		writeJSON(w, 200, map[string]any{"available": false})
		return
	}
	writeJSON(w, 200, map[string]any{"available": true, "maxPromptBytes": 262144,
		"images": map[string]any{"maxCount": 20, "maxInputBytes": 20 << 20, "mediaTypes": []string{"image/jpeg", "image/png", "image/webp", "image/gif"}},
		"tools":  map[string]any{"maxCount": 32}})
}
