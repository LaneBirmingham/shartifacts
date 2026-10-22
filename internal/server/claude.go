package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
)

// Claude answers window.claude.complete for artifacts. It exists only when the operator
// supplies an API key; without one the app needs no account or internet connection.
type Claude struct {
	client anthropic.Client
	model  string
	slots  chan struct{}
}

// Models that accept the server-side refusal fallback ("default" routes by category).
var fallbackModels = map[string]bool{"claude-fable-5-1": true, "claude-opus-5-5": true, "claude-opus-5": true, "claude-sonnet-5-5": true}

func NewClaude(apiKey, model string, opts ...option.RequestOption) *Claude {
	return &Claude{
		client: anthropic.NewClient(append([]option.RequestOption{option.WithAPIKey(apiKey)}, opts...)...),
		model:  model,
		slots:  make(chan struct{}, 4),
	}
}

// completion is one window.claude.complete call: a prompt, or a conversation of
// alternating user/assistant messages ending with the user.
type completion struct {
	Prompt   string `json:"prompt,omitempty"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages,omitempty"`
}

// messages validates the call and converts it to API messages.
func (in completion) messages() ([]anthropic.BetaMessageParam, error) {
	var messages []anthropic.BetaMessageParam
	switch {
	case in.Prompt != "" && in.Messages != nil:
		return nil, errors.New("send a prompt or messages, not both")
	case in.Prompt != "":
		messages = append(messages, anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(in.Prompt)))
	case len(in.Messages) > 0:
		for _, m := range in.Messages {
			role := anthropic.BetaMessageParamRoleUser
			switch {
			case m.Role == "assistant":
				role = anthropic.BetaMessageParamRoleAssistant
			case m.Role != "user":
				return nil, errors.New(`message roles must be "user" or "assistant"`)
			case strings.TrimSpace(m.Content) == "":
				return nil, errors.New("messages need content")
			}
			messages = append(messages, anthropic.BetaMessageParam{Role: role, Content: []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaTextBlock(m.Content)}})
		}
	default:
		return nil, errors.New("prompt is empty")
	}
	return messages, nil
}

func (c *Claude) Complete(ctx context.Context, messages []anthropic.BetaMessageParam) (string, error) {
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	params := anthropic.BetaMessageNewParams{Model: c.model, MaxTokens: 16000, Messages: messages}
	if fallbackModels[c.model] {
		params.Betas = []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01}
		params.Fallbacks = anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()}
	}
	resp, err := c.client.Beta.Messages.New(ctx, params)
	if err != nil {
		var apiErr *anthropic.Error
		if errors.As(err, &apiErr) {
			log.Printf("window.claude: %v", apiErr)
			switch apiErr.StatusCode {
			case 401, 403:
				return "", errors.New("the server's Anthropic API key was rejected")
			case 429:
				return "", errors.New("Claude is rate limited; try again shortly")
			case 400, 413:
				return "", fmt.Errorf("Claude rejected the request (%d); it may be too long", apiErr.StatusCode)
			}
			return "", fmt.Errorf("Claude is unavailable (%d)", apiErr.StatusCode)
		}
		return "", fmt.Errorf("cannot reach Claude: %w", err)
	}
	if resp.StopReason == anthropic.BetaStopReasonRefusal {
		return "", errors.New("Claude declined this request")
	}
	var text strings.Builder
	for _, block := range resp.Content {
		if t, ok := block.AsAny().(anthropic.BetaTextBlock); ok {
			text.WriteString(t.Text)
		}
	}
	return text.String(), nil
}

func (s *Server) complete(w http.ResponseWriter, r *http.Request) {
	if s.Claude == nil {
		writeJSON(w, 503, map[string]string{"error": "window.claude is off: start shartifacts with ANTHROPIC_API_KEY set to enable it"})
		return
	}
	var in completion
	if !decode(w, r, &in) {
		return
	}
	messages, e := in.messages()
	if e != nil {
		writeJSON(w, 400, map[string]string{"error": e.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
	defer cancel()
	text, e := s.Claude.Complete(ctx, messages)
	if e != nil {
		writeJSON(w, 502, map[string]string{"error": e.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"text": text})
}
