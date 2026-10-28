package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Connectors are the `mcp` capability's servers: MCP servers the operator lists in a JSON
// file (-connectors), which artifacts call through this server. Credentials stay here and
// never reach a page, as Claude's connectors keep the viewer's tokens from it.

type Connector struct {
	Name    string            `json:"name"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

type connectorHub struct {
	mu       sync.Mutex
	list     []Connector
	sessions map[string]*mcp.ClientSession
	tools    map[string][]*mcp.Tool
	listedAt map[string]time.Time
}

// LoadConnectors reads the connectors file: [{"name", "url", "headers"}].
func LoadConnectors(path string) ([]Connector, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var list []Connector
	if err = json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("read connectors: %w", err)
	}
	for _, c := range list {
		if c.Name == "" || c.Name == "artifacts_data" || !strings.HasPrefix(c.URL, "http") {
			return nil, fmt.Errorf("connector %q needs a name and an http(s) url", c.Name)
		}
	}
	return list, nil
}

type headerTransport struct {
	headers map[string]string
	base    http.RoundTripper
}

func (t headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range t.headers {
		r.Header.Set(k, v)
	}
	return t.base.RoundTrip(r)
}

type mcpFailure struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Server    string `json:"server,omitempty"`
	Retryable bool   `json:"retryable,omitempty"`
	Result    any    `json:"result,omitempty"`
}

func (e mcpFailure) Error() string { return e.Message }

func (h *connectorHub) find(name string) (Connector, bool) {
	for _, c := range h.list {
		if c.Name == name {
			return c, true
		}
	}
	return Connector{}, false
}

// session connects to a connector once and reuses the session. h.mu is not held.
func (h *connectorHub) session(ctx context.Context, name string) (*mcp.ClientSession, error) {
	c, ok := h.find(name)
	if !ok {
		return nil, mcpFailure{Code: "server_not_connected", Message: "no connector named " + name, Server: name}
	}
	h.mu.Lock()
	if s := h.sessions[name]; s != nil {
		h.mu.Unlock()
		return s, nil
	}
	h.mu.Unlock()
	client := mcp.NewClient(&mcp.Implementation{Name: "shartifacts", Version: appVersion}, nil)
	transport := &mcp.StreamableClientTransport{Endpoint: c.URL, HTTPClient: &http.Client{Timeout: 2 * time.Minute, Transport: headerTransport{c.Headers, http.DefaultTransport}}, MaxRetries: -1, DisableStandaloneSSE: true}
	s, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, connectFailure(name, err)
	}
	h.mu.Lock()
	if h.sessions == nil {
		h.sessions = map[string]*mcp.ClientSession{}
	}
	h.sessions[name] = s
	h.mu.Unlock()
	return s, nil
}

func connectFailure(name string, err error) error {
	if strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "403") || strings.Contains(strings.ToLower(err.Error()), "unauthorized") {
		return mcpFailure{Code: "needs_reauth", Message: name + " rejected its credentials; update the connectors file", Server: name}
	}
	return mcpFailure{Code: "server_unavailable", Message: name + " is unreachable: " + err.Error(), Server: name, Retryable: true}
}

// drop forgets a session that failed, so the next call reconnects.
func (h *connectorHub) drop(name string) {
	h.mu.Lock()
	if s := h.sessions[name]; s != nil {
		s.Close()
		delete(h.sessions, name)
	}
	h.mu.Unlock()
}

func (h *connectorHub) listTools(ctx context.Context, name string) ([]*mcp.Tool, error) {
	h.mu.Lock()
	if t, ok := h.tools[name]; ok && time.Since(h.listedAt[name]) < time.Minute {
		h.mu.Unlock()
		return t, nil
	}
	h.mu.Unlock()
	s, err := h.session(ctx, name)
	if err != nil {
		return nil, err
	}
	res, err := s.ListTools(ctx, nil)
	if err != nil {
		h.drop(name)
		return nil, connectFailure(name, err)
	}
	h.mu.Lock()
	if h.tools == nil {
		h.tools, h.listedAt = map[string][]*mcp.Tool{}, map[string]time.Time{}
	}
	h.tools[name], h.listedAt[name] = res.Tools, time.Now()
	h.mu.Unlock()
	return res.Tools, nil
}

func (h *connectorHub) call(ctx context.Context, name, tool string, input map[string]any) (map[string]any, error) {
	s, err := h.session(ctx, name)
	if err != nil {
		return nil, err
	}
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: input})
	if err != nil {
		h.drop(name)
		if errors.Is(err, context.Canceled) {
			return nil, mcpFailure{Code: "cancelled", Message: "cancelled", Server: name}
		}
		return nil, connectFailure(name, err)
	}
	b, _ := json.Marshal(res)
	var out map[string]any
	json.Unmarshal(b, &out)
	if res.IsError {
		msg := "the tool reported a failure"
		for _, c := range res.Content {
			if t, ok := c.(*mcp.TextContent); ok {
				msg = t.Text
				break
			}
		}
		return nil, mcpFailure{Code: "tool_error", Message: msg, Server: name, Result: out}
	}
	return out, nil
}

func (s *Server) connectorRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/connectors", func(w http.ResponseWriter, r *http.Request) {
		servers := []map[string]any{}
		for _, c := range s.connectors.list {
			entry := map[string]any{"server": c.Name, "kind": "connector", "authStatus": "connected", "tools": []any{}}
			tools, err := s.connectors.listTools(r.Context(), c.Name)
			var f mcpFailure
			switch {
			case errors.As(err, &f) && f.Code == "needs_reauth":
				entry["authStatus"] = "needs_reauth"
			case err != nil:
				entry["authStatus"] = "unknown"
			}
			list := []map[string]any{}
			for _, t := range tools {
				info := map[string]any{"name": t.Name, "description": t.Description}
				if t.Annotations != nil {
					info["annotations"] = t.Annotations
				}
				list = append(list, info)
			}
			entry["tools"] = list
			servers = append(servers, entry)
		}
		writeJSON(w, 200, map[string]any{"servers": servers})
	})
	mux.HandleFunc("POST /api/connectors/call", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Server string         `json:"server"`
			Tool   string         `json:"tool"`
			Input  map[string]any `json:"input"`
		}
		if !decode(w, r, &in) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		res, err := s.connectors.call(ctx, in.Server, in.Tool, in.Input)
		if err != nil {
			var f mcpFailure
			if !errors.As(err, &f) {
				f = mcpFailure{Code: "upstream_error", Message: err.Error(), Server: in.Server}
			}
			writeJSON(w, 502, f)
			return
		}
		writeJSON(w, 200, res)
	})
	mux.HandleFunc("POST /api/connectors/describe", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Server string `json:"server"`
			Tool   string `json:"tool"`
		}
		if !decode(w, r, &in) {
			return
		}
		tools, err := s.connectors.listTools(r.Context(), in.Server)
		if err != nil {
			var f mcpFailure
			errors.As(err, &f)
			writeJSON(w, 502, f)
			return
		}
		for _, t := range tools {
			if t.Name == in.Tool {
				writeJSON(w, 200, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema, "outputSchema": t.OutputSchema, "annotations": t.Annotations})
				return
			}
		}
		writeJSON(w, 502, mcpFailure{Code: "bad_request", Message: in.Server + " lists no tool " + in.Tool, Server: in.Server})
	})
}
