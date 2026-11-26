// Package codemode runs JavaScript that an agent writes against an API, the design
// Cloudflare calls Code Mode (https://blog.cloudflare.com/code-mode-mcp/). Rather than one
// MCP tool per operation, each of which costs every connected agent context, an agent gets
// two: search, whose code explores a spec of the operations, and execute, whose code calls
// them. One execute can chain several calls, loop over results and return only what the
// agent needs, so the intermediate results never pass through its context either.
//
// The code runs in a child process, the shartifacts binary re-executed with ChildArg, which
// holds a goja JavaScript runtime. The runtime has no files, network, environment or Node
// APIs: what it can reach is the globals a Job gives it and the operations, each of which
// the child sends back to this process to run. A separate process makes the limits hard —
// a run that loops forever or allocates without end is stopped or killed, and its crash
// cannot take the server down with it.
package codemode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ChildArg is the argv[1] that turns the shartifacts binary into the child that runs code.
const ChildArg = "__code-mode"

// Limits bound one run. A zero field takes its value from DefaultLimits.
type Limits struct {
	// CPU is how long the code may spend running JavaScript. Time spent waiting on an
	// operation or a timer does not count, so a run can deploy an app or wait for a change
	// without using it up.
	CPU time.Duration
	// Wall bounds the whole run, waits included.
	Wall time.Duration
	// Memory is the most heap the child may hold, in bytes.
	Memory int64
	// Calls is the most operations one run may call.
	Calls int
}

// DefaultLimits are generous for code that orchestrates calls, and small enough that a
// mistake costs a home server little.
var DefaultLimits = Limits{CPU: 30 * time.Second, Wall: 10 * time.Minute, Memory: 256 << 20, Calls: 1000}

func (l Limits) withDefaults() Limits {
	if l.CPU <= 0 {
		l.CPU = DefaultLimits.CPU
	}
	if l.Wall <= 0 {
		l.Wall = DefaultLimits.Wall
	}
	if l.Memory <= 0 {
		l.Memory = DefaultLimits.Memory
	}
	if l.Calls <= 0 {
		l.Calls = DefaultLimits.Calls
	}
	return l
}

// Job is one piece of code to run.
type Job struct {
	// Code is the agent's source: an async arrow function, as the tools ask for, or
	// statements, which run as the body of one. TypeScript annotations are stripped.
	Code string
	// Globals are JSON values the code sees as global variables, such as the spec that
	// search explores.
	Globals map[string]json.RawMessage
	// API names the global object whose methods are Ops; empty gives the code none.
	API string
	Ops []string
	// Limits bound the run.
	Limits Limits
}

// Call runs one operation for the code. args is the JSON object the code passed, and the
// JSON value returned is what its promise resolves to; an error rejects the promise with
// the error's message.
type Call func(ctx context.Context, op string, args json.RawMessage) (json.RawMessage, error)

// Result is what a run produced.
type Result struct {
	// Value is the JSON of what the function returned, or nil when it returned nothing.
	Value json.RawMessage
	// Error is set when the code failed: it did not parse, threw, or ran out of time or
	// memory. It is the message with the stack where there is one.
	Error string
	// Logs are the lines the code wrote with console.
	Logs []string
	// Calls are the operations it called, in order.
	Calls []CallRecord
}

// CallRecord is one operation a run called, and its error if it failed.
type CallRecord struct {
	Op    string `json:"op"`
	Error string `json:"error,omitempty"`
}

// DefaultQueue is how long a run waits for a free slot before it is refused.
const DefaultQueue = 30 * time.Second

// Runner starts runs. Its zero value is ready to use.
type Runner struct {
	// Self is the shartifacts binary to re-execute; empty means os.Executable.
	Self string
	// Concurrent is how many runs may go at once (default 4); later ones wait their turn.
	Concurrent int
	// Queue is how long a run waits for its turn (default DefaultQueue). A slot is held
	// for a whole run, waits on operations and timers included, so runs that wait a long
	// time could otherwise stall every other caller until its request gave up.
	Queue time.Duration

	once  sync.Once
	slots chan struct{}
}

// The wire between the processes is one JSON message per line. The child sends log, call,
// done and fail; this process answers each call with ok or err.
type message struct {
	T    string          `json:"t"`
	Op   string          `json:"op,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
	Text string          `json:"text,omitempty"`
}

// jobWire is the first line the child reads.
type jobWire struct {
	Code    string                     `json:"code"`
	Globals map[string]json.RawMessage `json:"globals,omitempty"`
	API     string                     `json:"api,omitempty"`
	Ops     []string                   `json:"ops,omitempty"`
	CPUms   int64                      `json:"cpu_ms"`
	WallMs  int64                      `json:"wall_ms"`
	Memory  int64                      `json:"memory"`
	Calls   int                        `json:"calls"`
}

// The sizes one message may carry. maxArgs is what /mcp accepts as a whole request, so code
// can send an operation no more than a tool call could. maxData bounds an operation's
// result handed to the code and the value the code returns: a child holds its parsed copy
// within its memory limit, and an agent is shown at most a megabyte of the answer anyway.
const (
	maxArgs = 12 << 20
	maxData = 32 << 20
	maxLine = maxData + 64<<10
)

// maxLogs bounds the console output kept from one run. Each line counts its newline, so
// a flood of empty lines is bounded too.
const maxLogs = 64 << 10

// encode writes m as one line. HTML characters stay as they are: escaping them as \u003c
// would make an HTML artifact's source several times larger on the wire.
func encode(w *bufio.Writer, m any) error {
	e := json.NewEncoder(w)
	e.SetEscapeHTML(false)
	if err := e.Encode(m); err != nil {
		return err
	}
	return w.Flush()
}

// Run runs job in a child process, serving its operations with call. The error is for
// failures to run at all; the code's own failures are in Result.Error.
func (r *Runner) Run(ctx context.Context, job Job, call Call) (*Result, error) {
	lim := job.Limits.withDefaults()
	r.once.Do(func() {
		n := r.Concurrent
		if n <= 0 {
			n = 4
		}
		r.slots = make(chan struct{}, n)
	})
	queue := r.Queue
	if queue <= 0 {
		queue = DefaultQueue
	}
	wait := time.NewTimer(queue)
	defer wait.Stop()
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	case <-wait.C:
		return nil, fmt.Errorf("code mode is busy: %d runs are going and none finished within %s; try again shortly", cap(r.slots), queue)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	self := r.Self
	if self == "" {
		var err error
		if self, err = os.Executable(); err != nil {
			return nil, fmt.Errorf("code mode needs its own binary to run code: %w", err)
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, lim.Wall)
	defer cancel()
	cmd := exec.CommandContext(runCtx, self, ChildArg)
	// The child gets no environment to leak: nothing the server was started with, such as
	// an API key, is there for code to find. GOMEMLIMIT makes its collector work hard as it
	// nears the limit, before the watchdog stops it at the limit itself.
	cmd.Env = []string{"GOMAXPROCS=2", "GOMEMLIMIT=" + strconv.FormatInt(lim.Memory, 10), "GOTRACEBACK=none"}
	cmd.SysProcAttr = procAttr()
	stderr := &headBuffer{max: 4 << 10}
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting the code sandbox: %w", err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()
	in := bufio.NewWriter(stdin)
	send := func(m any) error { return encode(in, m) }
	if err := send(jobWire{Code: job.Code, Globals: job.Globals, API: job.API, Ops: job.Ops, CPUms: lim.CPU.Milliseconds(), WallMs: lim.Wall.Milliseconds(), Memory: lim.Memory, Calls: lim.Calls}); err != nil {
		return nil, fmt.Errorf("starting the code sandbox: %w", err)
	}

	res := &Result{}
	logBytes := 0
	out := bufio.NewReaderSize(stdout, 1<<20)
	for {
		line, err := readLine(out, maxLine)
		if err != nil {
			// The child stopped without a word: it crashed, was killed for the wall limit,
			// or the caller went away.
			cmd.Process.Kill()
			cmd.Wait()
			switch {
			case errors.Is(err, errLineTooLong):
				res.Error = fmt.Sprintf("the code sent a message larger than %d MiB", maxLine>>20)
			case errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
				res.Error = fmt.Sprintf("the run took longer than %s and was stopped", lim.Wall)
			case ctx.Err() != nil:
				return nil, ctx.Err()
			case strings.Contains(stderr.String(), "out of memory"):
				res.Error = fmt.Sprintf("the code ran out of memory (the limit is %d MiB)", lim.Memory>>20)
			default:
				res.Error = "the code sandbox stopped unexpectedly"
				if s := strings.TrimSpace(stderr.String()); s != "" {
					res.Error += ": " + s
				}
			}
			return res, nil
		}
		var m message
		if err := json.Unmarshal(line, &m); err != nil {
			res.Error = "the code sandbox sent something unreadable"
			return res, nil
		}
		switch m.T {
		case "log":
			if logBytes < maxLogs {
				line := truncate(m.Text, maxLogs-logBytes)
				logBytes += len(line) + 1
				res.Logs = append(res.Logs, line)
				if logBytes >= maxLogs {
					res.Logs = append(res.Logs, "… later console output was dropped (64 KiB kept)")
				}
			}
		case "call":
			// The child refuses calls past the limit and oversized arguments itself, so they
			// cost the code JavaScript time rather than a round trip each; these checks are
			// the backstop.
			if len(res.Calls) >= lim.Calls || len(m.Data) > maxArgs {
				res.Error = fmt.Sprintf("the code called more than %d operations, or sent one more than %d MiB, and was stopped", lim.Calls, maxArgs>>20)
				return res, nil
			}
			reply := message{T: "ok"}
			if call == nil {
				reply = message{T: "err", Text: "this run has no operations"}
			} else {
				value, err := call(runCtx, m.Op, m.Data)
				if err == nil && len(value) > maxData {
					err = fmt.Errorf("%s returned %d MiB, more than the %d MiB an operation can hand to code: ask for less, such as one version rather than the history", m.Op, len(value)>>20, maxData>>20)
				}
				if err != nil {
					reply = message{T: "err", Text: err.Error()}
					res.Calls = append(res.Calls, CallRecord{Op: m.Op, Error: err.Error()})
				} else {
					reply.Data = value
					res.Calls = append(res.Calls, CallRecord{Op: m.Op})
				}
			}
			if err := send(reply); err != nil {
				// The child is gone; the next read says why.
				continue
			}
		case "done":
			res.Value = m.Data
			return res, nil
		case "fail":
			res.Error = m.Text
			return res, nil
		}
	}
}

var errLineTooLong = errors.New("line too long")

// truncate cuts s to at most n bytes, at a character boundary, marking the cut.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// readLine reads one newline-terminated line of at most max bytes, without the newline.
func readLine(r *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(line)+len(chunk) > max {
			return nil, errLineTooLong
		}
		line = append(line, chunk...)
		switch {
		case err == nil:
			return line[:len(line)-1], nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(line) > 0:
			return nil, io.ErrUnexpectedEOF
		default:
			return nil, err
		}
	}
}

// headBuffer keeps the first max bytes written to it: the child's fatal error is the first
// thing it prints.
type headBuffer struct {
	mu  sync.Mutex
	max int
	buf bytes.Buffer
}

func (h *headBuffer) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if room := h.max - h.buf.Len(); room > 0 {
		h.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (h *headBuffer) String() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.buf.String()
}
