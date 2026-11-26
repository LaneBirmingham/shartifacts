package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/LaneBirmingham/shartifacts/internal/codemode"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The tool lists /mcp can offer. Code mode, the default, is Cloudflare's design
// (https://blog.cloudflare.com/code-mode-mcp/): two tools, search and execute, in place of
// one per operation, so a connection costs an agent a fraction of the context and one
// execute can do the work of many tool calls. Tools mode is one tool per operation, for
// clients that cannot write code or would rather not.
const (
	mcpCode  = "code"
	mcpTools = "tools"
)

// mcpMode is the tool list a request asks for: ?mode=code or ?mode=tools on the /mcp URL,
// or the server's MCPMode. An unknown mode is refused rather than quietly replaced, so a
// typo in a client's configuration shows.
func (s *Server) mcpMode(r *http.Request) (string, error) {
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = s.MCPMode
	}
	switch mode {
	case "", mcpCode:
		return mcpCode, nil
	case mcpTools:
		return mcpTools, nil
	}
	return "", fmt.Errorf("unknown MCP mode %q: use mode=code (search and execute) or mode=tools (one tool per operation)", mode)
}

// codeModeSummary is the line code mode adds to the instructions every connection gets.
const codeModeSummary = "Two tools: execute runs an async JavaScript arrow function in a sandbox where each operation named here is a method of shartifacts, e.g. async () => (await shartifacts.list_artifacts({limit: 5})).artifacts.map(a => a.title); chain calls in one run and return only what you need. search runs one against spec to look up an operation's inputs (spec.operations.update_artifact.signature) or the guide (spec.guide)."

// maxCodeResult bounds what one search or execute returns: what an agent asks for is what
// enters its context, and past this much it asked for too much.
const maxCodeResult = 1 << 20

// searchLimits bound a search. It only reads the spec, so it needs neither timers nor long
// waits, and its own pool of sandboxes keeps it answering while executes wait on changes.
var searchLimits = codemode.Limits{Wall: time.Minute}

// mcpWriteTime is how long /mcp may take to answer: an execute may wait its turn and then
// run for its wall limit, past the server's usual write timeout, and an answer lost to that
// timeout would leave the agent not knowing what its code changed.
func mcpWriteTime() time.Duration {
	return codemode.DefaultQueue + codemode.DefaultLimits.Wall + time.Minute
}

// codeInput is what search and execute take.
type codeInput struct {
	Code string `json:"code" jsonschema:"A JavaScript async arrow function, e.g. async () => { ... }; what it returns comes back, a string as itself and anything else as JSON"`
}

// codeServer offers a connection's tools the Code Mode way. m is the server to offer them
// on, and ts the connection's tools, whose own server execute calls them through.
func (s *Server) codeServer(m *mcp.Server, ts *toolset, guide string) *mcp.Server {
	names := make([]string, len(ts.ops))
	for i, op := range ts.ops {
		names[i] = op.tool.Name
	}
	spec := sync.OnceValues(func() (json.RawMessage, error) { return codeSpec(ts, guide) })
	readOnly, destructive := true, true
	mcp.AddTool(m, &mcp.Tool{
		Name:        "search",
		Description: "Look up the shartifacts API with JavaScript. code is an async arrow function run against the global spec: spec.operations maps each operation execute can call to {description, signature (TypeScript), input (JSON Schema, refs inline), destructive}, and spec.guide is the full guide as an array of paragraphs. Return just what you need, e.g. async () => spec.operations.create_artifact.signature, or async () => spec.guide.filter(p => /upload/i.test(p)).",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: readOnly},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in codeInput) (*mcp.CallToolResult, any, error) {
		raw, err := spec()
		if err != nil {
			return nil, nil, err
		}
		res, err := s.codeSearch.Run(ctx, codemode.Job{Code: in.Code, Globals: map[string]json.RawMessage{"spec": raw}, Limits: searchLimits}, nil)
		return codeResult(res, err)
	})
	lim := codemode.DefaultLimits
	mcp.AddTool(m, &mcp.Tool{
		Name: "execute",
		Description: "Run JavaScript against shartifacts. code is an async arrow function; each operation is a method of the global shartifacts that takes one object and resolves to its JSON result, or throws with the reason: async () => { const a = await shartifacts.get_artifact({id: \"…\"}); return a.revisions[0].version }. " +
			"Chain calls, loop and filter in one run, and return only what you need; console output comes back too. " +
			"Operations: " + strings.Join(names, ", ") + ". search has each one's inputs. " +
			fmt.Sprintf("The sandbox has no network, files or Node APIs (setTimeout, atob and btoa work) and %s of JavaScript time and %d MiB per run; waiting on operations is free. ", lim.CPU, lim.Memory>>20) +
			"A confirmation token (delete_artifact, manage_app delete) cannot be used in the run that got it, nor can a run delete what it archived: return it, ask the person, then confirm in another execute.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &destructive},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in codeInput) (*mcp.CallToolResult, any, error) {
		d := &dispatcher{server: ts.server}
		defer d.close()
		res, err := s.code.Run(ctx, codemode.Job{Code: in.Code, API: "shartifacts", Ops: names}, d.call)
		return codeResult(res, err)
	})
	return m
}

// codeSpec is the spec search explores: each operation, in the order the classic tool list
// has them, and the guide.
func codeSpec(ts *toolset, guide string) (json.RawMessage, error) {
	type specOp struct {
		Description string `json:"description"`
		Signature   string `json:"signature"`
		Input       any    `json:"input"`
		Destructive bool   `json:"destructive,omitempty"`
	}
	var b bytes.Buffer
	b.WriteString(`{"server":`)
	name, _ := json.Marshal("shartifacts " + appVersion)
	b.Write(name)
	b.WriteString(`,"operations":{`)
	for i, op := range ts.ops {
		if i > 0 {
			b.WriteByte(',')
		}
		key, _ := json.Marshal(op.tool.Name)
		destructive := op.tool.Annotations != nil && op.tool.Annotations.DestructiveHint != nil && *op.tool.Annotations.DestructiveHint
		v, err := json.Marshal(specOp{op.tool.Description, codemode.Signature(op.tool.Name, op.tool.Description, op.input), op.input, destructive})
		if err != nil {
			return nil, fmt.Errorf("spec for %s: %w", op.tool.Name, err)
		}
		b.Write(key)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteString(`},"guide":`)
	paragraphs := []string{}
	for _, p := range strings.Split(guide, "\n\n") {
		if p = strings.TrimSpace(p); p != "" {
			paragraphs = append(paragraphs, p)
		}
	}
	g, _ := json.Marshal(paragraphs)
	b.Write(g)
	b.WriteByte('}')
	return b.Bytes(), nil
}

// dispatcher answers one execute's operations. It calls the tool of the same name on the
// connection's classic server, through an in-process session opened at the first call, so
// an operation is validated and answered exactly as that tool would be. The runner serves
// a run's calls one at a time, so it needs no lock.
type dispatcher struct {
	server  *mcp.Server
	client  *mcp.ClientSession
	session *mcp.ServerSession
	// issued are the confirmation tokens this run was handed, and archived the artifacts
	// it archived.
	issued   map[string]bool
	archived map[string]bool
}

// The two calls of a deletion are for the person to say yes in between: as separate tool
// calls they could; inside one run of code they could not. So a run may not redeem a
// confirmation it was handed, nor delete what it archived, since deleting an archived
// artifact needs no confirmation (archiving was meant to be the first, visible step).
var (
	errConfirmInRun = errors.New("that confirmation was handed out in this same run, so the person has not seen it: return the confirm token, ask them, and confirm in a new execute once they agree")
	errDeleteInRun  = errors.New("this run archived that artifact, so the person has not seen it go: return what you archived, ask them, and delete it in a new execute once they agree")
)

// target is the artifacts an operation's arguments name, and what it asks of them.
type target struct {
	ID       string   `json:"id"`
	IDs      []string `json:"ids"`
	Confirm  string   `json:"confirm"`
	Archived *bool    `json:"archived"`
}

func (t target) names(set map[string]bool) bool {
	if set[t.ID] {
		return true
	}
	for _, id := range t.IDs {
		if set[id] {
			return true
		}
	}
	return false
}

// connect opens the session. Its frames are not capped as the SDK's in-memory transport's
// are, at 16 MiB: both ends are this process, the code's arguments are already bounded by
// the sandbox, and a result is as large as the same tool would answer over HTTP.
func (d *dispatcher) connect(ctx context.Context) error {
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()
	session, err := d.server.Connect(ctx, &mcp.IOTransport{Reader: serverIn, Writer: serverOut, MaxLineLength: -1}, nil)
	if err != nil {
		return err
	}
	client, err := mcp.NewClient(&mcp.Implementation{Name: "shartifacts-code-mode", Version: appVersion}, nil).Connect(ctx, &mcp.IOTransport{Reader: clientIn, Writer: clientOut, MaxLineLength: -1}, nil)
	if err != nil {
		session.Close()
		return err
	}
	d.session, d.client = session, client
	return nil
}

func (d *dispatcher) call(ctx context.Context, op string, args json.RawMessage) (json.RawMessage, error) {
	var asked target
	json.Unmarshal(args, &asked)
	if asked.Confirm != "" && d.issued[asked.Confirm] {
		return nil, errConfirmInRun
	}
	if op == "delete_artifact" && asked.names(d.archived) {
		return nil, errDeleteInRun
	}
	if d.client == nil {
		if err := d.connect(ctx); err != nil {
			return nil, err
		}
	}
	res, err := d.client.CallTool(ctx, &mcp.CallToolParams{Name: op, Arguments: args})
	if err != nil {
		// The session may be what failed; the run's next call opens a fresh one.
		d.close()
		return nil, err
	}
	var text strings.Builder
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			if text.Len() > 0 {
				text.WriteByte('\n')
			}
			text.WriteString(t.Text)
		}
	}
	if res.IsError {
		return nil, errors.New(text.String())
	}
	if res.StructuredContent == nil {
		// A tool that answers in text, such as get_design_system: the operation resolves to
		// that text as a string.
		return json.Marshal(text.String())
	}
	// The text is the structured result as the tool marshalled it, fields in their order;
	// the decoded copy is the fallback.
	out := json.RawMessage(text.String())
	if len(res.Content) != 1 || !json.Valid(out) {
		if out, err = json.Marshal(res.StructuredContent); err != nil {
			return nil, err
		}
	}
	var handed struct {
		Confirm string `json:"confirm"`
	}
	if json.Unmarshal(out, &handed) == nil && handed.Confirm != "" {
		if d.issued == nil {
			d.issued = map[string]bool{}
		}
		d.issued[handed.Confirm] = true
	}
	if op == "update_artifact" && asked.Archived != nil && *asked.Archived {
		// Every artifact asked for, archived or not: one that failed was not deleted
		// either, and is not deletable without a confirmation.
		if d.archived == nil {
			d.archived = map[string]bool{}
		}
		for _, id := range append([]string{asked.ID}, asked.IDs...) {
			if id != "" {
				d.archived[id] = true
			}
		}
	}
	return out, nil
}

func (d *dispatcher) close() {
	if d.client != nil {
		d.client.Close()
	}
	if d.session != nil {
		d.session.Close()
	}
	d.client, d.session = nil, nil
}

// codeResult is search's or execute's answer: what the code returned (a string as itself,
// anything else as JSON), or why it failed and what it had called by then; then its
// console output, if any.
func codeResult(res *codemode.Result, err error) (*mcp.CallToolResult, any, error) {
	if err != nil {
		return nil, nil, err
	}
	out := &mcp.CallToolResult{}
	var text string
	switch {
	case res.Error != "":
		out.IsError = true
		text = res.Error
		if len(res.Calls) > 0 {
			text += "\n\nOperations it called before stopping: " + describeCalls(res.Calls)
		}
	case res.Value == nil:
		text = "undefined: the function returned nothing (return the values you need)"
	default:
		var str string
		if json.Unmarshal(res.Value, &str) == nil {
			text = str
		} else {
			text = string(res.Value)
		}
	}
	if len(text) > maxCodeResult {
		cut := maxCodeResult
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut] + fmt.Sprintf("\n… cut at %d KiB of %d KiB: return less, such as the fields or the part of the source you need", maxCodeResult>>10, len(text)>>10)
	}
	out.Content = []mcp.Content{&mcp.TextContent{Text: text}}
	if len(res.Logs) > 0 {
		out.Content = append(out.Content, &mcp.TextContent{Text: "console:\n" + strings.Join(res.Logs, "\n")})
	}
	return out, nil, nil
}

// describeCalls lists a failed run's operations, so an agent knows what it already changed.
func describeCalls(calls []codemode.CallRecord) string {
	const most = 30
	parts := []string{}
	for i, c := range calls {
		if i == most {
			parts = append(parts, fmt.Sprintf("and %d more", len(calls)-most))
			break
		}
		if c.Error != "" {
			msg := c.Error
			if len(msg) > 120 {
				cut := 120
				for cut > 0 && !utf8.RuneStart(msg[cut]) {
					cut--
				}
				msg = msg[:cut] + "…"
			}
			parts = append(parts, c.Op+" (failed: "+msg+")")
		} else {
			parts = append(parts, c.Op)
		}
	}
	return strings.Join(parts, ", ")
}
