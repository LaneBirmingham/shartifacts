package codemode

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/metrics"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
	"github.com/dop251/goja/parser"
	"github.com/evanw/esbuild/pkg/api"
)

// Child runs in the re-executed binary: it reads one job from stdin, runs it, and exits.
// It never returns.
func Child() {
	if err := child(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "code mode:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// Exit codes the watchdogs use when they stop the child; the message on the wire says why.
const (
	exitMemory = 3
	exitCPU    = 4
	exitWall   = 5
	exitParent = 6
)

// wire is the child's side of the pipe. The watchdogs write to it from their own
// goroutines, so writes take turns.
type wire struct {
	mu sync.Mutex
	r  *bufio.Reader
	w  *bufio.Writer
}

func (w *wire) send(m message) error {
	if m.T == "fail" {
		// An error's message is the code's own string, so it could be any size.
		m.Text = truncate(m.Text, maxLogs)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return encode(w.w, m)
}

// call sends an operation to the parent and waits for its reply. Only the runtime's
// goroutine reads, so reads need no lock.
func (w *wire) call(op string, args json.RawMessage) (message, error) {
	if err := w.send(message{T: "call", Op: op, Data: args}); err != nil {
		return message{}, err
	}
	line, err := readLine(w.r, maxLine)
	if err != nil {
		return message{}, err
	}
	var m message
	err = json.Unmarshal(line, &m)
	return m, err
}

func child(stdin io.Reader, stdout io.Writer) error {
	w := &wire{r: bufio.NewReaderSize(stdin, 1<<20), w: bufio.NewWriterSize(stdout, 64<<10)}
	line, err := readLine(w.r, maxLine)
	if err != nil {
		return err
	}
	var job jobWire
	if err := json.Unmarshal(line, &job); err != nil {
		return err
	}
	go watchMemory(job.Memory, w)
	// The parent kills the child at the wall limit, and on Linux the kernel kills it if the
	// parent dies first. Elsewhere nothing would, so the child keeps its own deadline and
	// watches for being orphaned.
	if job.WallMs > 0 {
		time.AfterFunc(time.Duration(job.WallMs)*time.Millisecond+2*time.Second, func() { os.Exit(exitWall) })
	}
	go watchParent()
	r := &run{wire: w, vm: goja.New(), maxCalls: job.Calls}
	// eval and new Function compile with the runtime's options, and by default those load
	// the source map a sourceMappingURL comment names from a file path: code could probe
	// the server's files, or block on a FIFO. The program itself is compiled with
	// noSourceMapFiles.
	r.vm.SetParserOptions(parser.WithDisableSourceMaps)
	r.cpu = newBudget(time.Duration(job.CPUms)*time.Millisecond, func() {
		r.vm.Interrupt(fmt.Sprintf("the code ran JavaScript for more than %s (waiting on operations and timers does not count)", time.Duration(job.CPUms)*time.Millisecond))
	}, func() {
		// The runtime did not stop when interrupted: it is stuck in Go, such as a regular
		// expression backtracking. Only leaving stops it.
		w.send(message{T: "fail", Text: fmt.Sprintf("the code ran JavaScript for more than %s and did not stop", time.Duration(job.CPUms)*time.Millisecond)})
		os.Exit(exitCPU)
	})
	return r.run(job)
}

// run is one job's runtime.
type run struct {
	wire   *wire
	vm     *goja.Runtime
	cpu    *budget
	timers []*timer
	nextID int64
	logged int
	// calls counts the operations called; past maxCalls they are refused here.
	calls, maxCalls int
}

type timer struct {
	id   int64
	due  time.Time
	fn   goja.Callable
	args []goja.Value
}

func (r *run) run(job jobWire) error {
	vm := r.vm
	// Recursion deep enough for any real use; a runaway one stops here, not at the memory limit.
	vm.SetMaxCallStackSize(2000)
	r.installConsole()
	r.installTimers()
	r.installBase64()
	for name, raw := range job.Globals {
		v, err := r.parseJSON(raw)
		if err != nil {
			return fmt.Errorf("global %s: %w", name, err)
		}
		vm.Set(name, v)
	}
	if job.API != "" {
		vm.Set(job.API, r.api(job.API, job.Ops))
	}
	prg, err := compile(job.Code)
	if err != nil {
		return r.wire.send(message{T: "fail", Text: err.Error()})
	}
	// The budget runs until the answer is ready: describing an error or serialising the
	// result can run the code's own getters and toJSON.
	r.cpu.start()
	out := r.finish(vm.RunProgram(prg))
	r.cpu.stop()
	return r.wire.send(out)
}

// finish runs the program's function to the end and makes the message that answers it.
func (r *run) finish(v goja.Value, err error) message {
	if err == nil {
		if fn, ok := goja.AssertFunction(v); ok {
			v, err = fn(goja.Undefined())
		}
	}
	// Operations resolve as they are called, so the promise settles as soon as the code
	// stops awaiting. What can keep it pending is a timer, so run those in turn.
	for err == nil {
		p, ok := v.Export().(*goja.Promise)
		if !ok {
			break
		}
		if p.State() == goja.PromiseStateFulfilled {
			v = p.Result()
			break
		}
		if p.State() == goja.PromiseStateRejected {
			return message{T: "fail", Text: r.describe(p.Result())}
		}
		if len(r.timers) == 0 {
			return message{T: "fail", Text: "the function's promise never settled: it awaited something that cannot resolve here (there is no network, and timers and operations resolve on their own)"}
		}
		err = r.fireTimer()
	}
	if err != nil {
		return message{T: "fail", Text: r.describeError(err)}
	}
	if goja.IsUndefined(v) {
		return message{T: "done"}
	}
	s, err := r.stringify(v)
	if err != nil {
		return message{T: "fail", Text: "the return value cannot be sent as JSON: " + r.describeError(err)}
	}
	if s == "" {
		return message{T: "done"}
	}
	if len(s) > maxData {
		return message{T: "fail", Text: fmt.Sprintf("the return value is %d MiB as JSON, more than the %d MiB a run can return: return less, such as the fields you need", len(s)>>20, maxData>>20)}
	}
	return message{T: "done", Data: json.RawMessage(s)}
}

// compile accepts the shapes agents write: a function expression (the documented one),
// statements to run as an async function's body, or either with TypeScript annotations.
func compile(code string) (*goja.Program, error) {
	code = strings.TrimRight(strings.TrimSpace(code), "; \t\r\n")
	if code == "" {
		return nil, errors.New("SyntaxError: no code: send an async arrow function, such as async () => { return 1 }")
	}
	// Each wrapper starts on the code's first line, so line numbers in errors stay the
	// agent's own.
	asExpression, asBody := "("+code+"\n)", "(async () => {"+code+"\n})"
	prg, exprErr := compileSource(asExpression)
	if exprErr == nil {
		return prg, nil
	}
	if prg, err := compileSource(asBody); err == nil {
		return prg, nil
	}
	// TypeScript: esbuild strips the types, and its inline source map keeps errors
	// pointing at the agent's lines.
	for _, src := range []string{asExpression, asBody} {
		out := api.Transform(src, api.TransformOptions{Loader: api.LoaderTS, Sourcefile: "code", Sourcemap: api.SourceMapInline, Target: api.ESNext})
		if len(out.Errors) == 0 {
			if prg, err := compileSource(string(out.Code)); err == nil {
				return prg, nil
			}
		}
	}
	return nil, fmt.Errorf("SyntaxError: %v", strings.TrimPrefix(exprErr.Error(), "SyntaxError: "))
}

// noSourceMapFiles refuses source maps named by path: only the inline map esbuild writes is
// read, never a file a sourceMappingURL comment names.
func noSourceMapFiles(string) ([]byte, error) {
	return nil, errors.New("source maps are read only from data: URLs")
}

func compileSource(src string) (*goja.Program, error) {
	ast, err := goja.Parse("code", src, parser.WithSourceMapLoader(noSourceMapFiles))
	if err != nil {
		return nil, err
	}
	return goja.CompileAST(ast, false)
}

// api builds the object whose methods are the operations. Each takes one object of
// arguments and returns a promise of the operation's JSON result.
func (r *run) api(name string, ops []string) goja.Value {
	vm := r.vm
	target := vm.NewObject()
	known := map[string]bool{}
	for _, op := range ops {
		known[op] = true
		target.Set(op, r.operation(name, op))
	}
	sorted := append([]string(nil), ops...)
	sort.Strings(sorted)
	// A proxy, so a misspelt operation fails with the list of real ones rather than
	// "undefined is not a function".
	return vm.ToValue(vm.NewProxy(target, &goja.ProxyTrapConfig{
		Get: func(t *goja.Object, prop string, receiver goja.Value) goja.Value {
			if known[prop] {
				return t.Get(prop)
			}
			if v := t.Get(prop); v != nil || prop == "then" || prop == "toJSON" {
				return v
			}
			panic(vm.NewTypeError("%s has no operation %q; it has %s (search shows their inputs)", name, prop, strings.Join(sorted, ", ")))
		},
	}))
}

func (r *run) operation(api, op string) func(goja.FunctionCall) goja.Value {
	vm := r.vm
	return func(fc goja.FunctionCall) goja.Value {
		args := "{}"
		if a := fc.Argument(0); !goja.IsUndefined(a) && !goja.IsNull(a) {
			obj, ok := a.(*goja.Object)
			if !ok || obj.ClassName() == "Array" || obj.ClassName() == "Function" {
				panic(vm.NewTypeError("%s.%s takes one object of arguments, such as {id: \"…\"}", api, op))
			}
			s, err := r.stringify(a)
			if err != nil {
				panic(vm.NewTypeError("%s.%s: its arguments cannot be sent as JSON: %s", api, op, r.describeError(err)))
			}
			if len(s) > maxArgs {
				panic(vm.NewTypeError("%s.%s: its arguments are %d MiB as JSON, more than the %d MiB an operation takes; send large files with list_media's upload URL", api, op, len(s)>>20, maxArgs>>20))
			}
			args = s
		}
		promise, resolve, reject := vm.NewPromise()
		if r.calls >= r.maxCalls {
			// Refused here, so a loop that ignores the refusal spends its JavaScript time
			// rather than waiting on round trips to the parent.
			e := r.newError(fmt.Sprintf("this run has made its %d calls; return what you have and continue in another", r.maxCalls))
			e.Set("operation", op)
			reject(e)
			return vm.ToValue(promise)
		}
		r.calls++
		r.cpu.pause()
		reply, err := r.wire.call(op, json.RawMessage(args))
		r.cpu.resume()
		if err != nil {
			// The parent is gone; there is no one left to answer.
			os.Exit(1)
		}
		if reply.T == "ok" {
			v, err := r.parseJSON(reply.Data)
			if err != nil {
				reject(vm.NewGoError(err))
			} else {
				resolve(v)
			}
		} else {
			e := r.newError(reply.Text)
			e.Set("operation", op)
			reject(e)
		}
		return vm.ToValue(promise)
	}
}

func (r *run) newError(msg string) *goja.Object {
	ctor, _ := goja.AssertConstructor(r.vm.Get("Error"))
	e, err := ctor(nil, r.vm.ToValue(msg))
	if err != nil {
		return r.vm.NewGoError(errors.New(msg))
	}
	return e
}

func (r *run) parseJSON(raw json.RawMessage) (goja.Value, error) {
	if len(raw) == 0 {
		return goja.Undefined(), nil
	}
	parse, _ := goja.AssertFunction(r.vm.Get("JSON").ToObject(r.vm).Get("parse"))
	v, err := parse(goja.Undefined(), r.vm.ToValue(string(raw)))
	r.rearm(err)
	return v, err
}

// stringify is JSON.stringify; "" means the value has no JSON form (a function, say).
func (r *run) stringify(v goja.Value) (string, error) {
	fn, _ := goja.AssertFunction(r.vm.Get("JSON").ToObject(r.vm).Get("stringify"))
	s, err := fn(goja.Undefined(), v)
	if err != nil {
		r.rearm(err)
		return "", err
	}
	if goja.IsUndefined(s) {
		return "", nil
	}
	return s.String(), nil
}

// describe renders a thrown or rejected value: an Error with its stack, anything else as
// itself. Reading it runs the code's own getters and toString, which can throw in turn; it
// is read inside the runtime, so that comes back as an error rather than a panic here.
func (r *run) describe(v goja.Value) string {
	var s string
	read, _ := goja.AssertFunction(r.vm.ToValue(func(goja.FunctionCall) goja.Value {
		s = r.render(v)
		return goja.Undefined()
	}))
	if _, err := read(goja.Undefined()); err != nil {
		var interrupted *goja.InterruptedError
		if errors.As(err, &interrupted) {
			r.rearm(err)
			return fmt.Sprint(interrupted.Value())
		}
		return "uncaught exception (reading it threw again)"
	}
	return s
}

// rearm passes the CPU limit's interrupt on. A call into the runtime from Go, such as
// JSON.stringify for console.log, returns the interrupt as an error, and the code that made
// the call would otherwise carry on running.
func (r *run) rearm(err error) {
	var interrupted *goja.InterruptedError
	if errors.As(err, &interrupted) {
		r.vm.Interrupt(interrupted.Value())
	}
}

func (r *run) render(v goja.Value) string {
	if obj, ok := v.(*goja.Object); ok {
		if stack := obj.Get("stack"); stack != nil && !goja.IsUndefined(stack) {
			if s := stack.String(); s != "" {
				return s
			}
		}
		if obj.ClassName() != "Error" {
			if s, err := r.stringify(v); err == nil && s != "" {
				return "uncaught " + s
			}
		}
	}
	if v == nil || goja.IsUndefined(v) {
		return "uncaught undefined"
	}
	return v.String()
}

func (r *run) describeError(err error) string {
	var ex *goja.Exception
	var interrupted *goja.InterruptedError
	switch {
	case errors.As(err, &interrupted):
		return fmt.Sprint(interrupted.Value())
	case errors.As(err, &ex):
		return r.describe(ex.Value())
	}
	var stack *goja.StackOverflowError
	if errors.As(err, &stack) {
		return "RangeError: maximum call stack size exceeded"
	}
	return err.Error()
}

func (r *run) installConsole() {
	console := r.vm.NewObject()
	for _, level := range []string{"log", "info", "debug", "warn", "error"} {
		prefix := ""
		if level == "warn" || level == "error" {
			prefix = level + ": "
		}
		console.Set(level, func(fc goja.FunctionCall) goja.Value {
			if r.logged >= maxLogs {
				return goja.Undefined()
			}
			parts := make([]string, len(fc.Arguments))
			for i, a := range fc.Arguments {
				parts[i] = r.format(a)
			}
			line := truncate(prefix+strings.Join(parts, " "), maxLogs-r.logged)
			r.logged += len(line) + 1
			r.wire.send(message{T: "log", Text: line})
			return goja.Undefined()
		})
	}
	r.vm.Set("console", console)
}

// format renders one console argument as a browser console would in text: strings as they
// are, errors with their stack, other values as JSON.
func (r *run) format(v goja.Value) string {
	if v == nil || goja.IsUndefined(v) {
		return "undefined"
	}
	if obj, ok := v.(*goja.Object); ok {
		switch obj.ClassName() {
		case "Error":
			return r.describe(v)
		case "Function":
			return v.String()
		}
		if s, err := r.stringify(v); err == nil && s != "" {
			return s
		}
	}
	return v.String()
}

func (r *run) installTimers() {
	r.vm.Set("setTimeout", func(fc goja.FunctionCall) goja.Value {
		fn, ok := goja.AssertFunction(fc.Argument(0))
		if !ok {
			panic(r.vm.NewTypeError("setTimeout needs a function"))
		}
		ms := fc.Argument(1).ToFloat()
		if ms != ms || ms < 0 {
			ms = 0
		}
		r.nextID++
		var args []goja.Value
		if len(fc.Arguments) > 2 {
			// The runtime reuses the arguments' storage once this returns.
			args = append(args, fc.Arguments[2:]...)
		}
		r.timers = append(r.timers, &timer{id: r.nextID, due: time.Now().Add(time.Duration(ms * float64(time.Millisecond))), fn: fn, args: args})
		return r.vm.ToValue(r.nextID)
	})
	r.vm.Set("clearTimeout", func(fc goja.FunctionCall) goja.Value {
		id := fc.Argument(0).ToInteger()
		for i, t := range r.timers {
			if t.id == id {
				r.timers = append(r.timers[:i], r.timers[i+1:]...)
				break
			}
		}
		return goja.Undefined()
	})
}

// fireTimer waits for the earliest timer and calls it. Waiting does not use the CPU budget.
func (r *run) fireTimer() error {
	next := 0
	for i, t := range r.timers {
		if t.due.Before(r.timers[next].due) {
			next = i
		}
	}
	t := r.timers[next]
	r.timers = append(r.timers[:next], r.timers[next+1:]...)
	if wait := time.Until(t.due); wait > 0 {
		r.cpu.pause()
		time.Sleep(wait)
		r.cpu.resume()
	}
	_, err := t.fn(goja.Undefined(), t.args...)
	return err
}

// installBase64 adds atob and btoa, for code that encodes a media chunk or decodes data.
func (r *run) installBase64() {
	r.vm.Set("btoa", func(fc goja.FunctionCall) goja.Value {
		s := fc.Argument(0).String()
		b := make([]byte, 0, len(s))
		for _, c := range s {
			if c > 0xff {
				panic(r.vm.NewTypeError("btoa: the string has characters outside Latin-1"))
			}
			b = append(b, byte(c))
		}
		return r.vm.ToValue(base64.StdEncoding.EncodeToString(b))
	})
	r.vm.Set("atob", func(fc goja.FunctionCall) goja.Value {
		s := strings.Map(func(c rune) rune {
			if c == ' ' || c == '\n' || c == '\t' || c == '\r' {
				return -1
			}
			return c
		}, fc.Argument(0).String())
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			if b, err = base64.RawStdEncoding.DecodeString(s); err != nil {
				panic(r.vm.NewTypeError("atob: not base64"))
			}
		}
		runes := make([]rune, len(b))
		for i, c := range b {
			runes[i] = rune(c)
		}
		return r.vm.ToValue(string(runes))
	})
}

// budget is the CPU time a run may use. It runs only while JavaScript does: operations and
// timers pause it. At the limit it interrupts the runtime; two seconds later, if the
// runtime has not stopped, it gives up on it.
type budget struct {
	mu        sync.Mutex
	left      time.Duration
	since     time.Time
	running   bool
	soft      *time.Timer
	hard      *time.Timer
	interrupt func()
	kill      func()
}

func newBudget(limit time.Duration, interrupt, kill func()) *budget {
	return &budget{left: limit, interrupt: interrupt, kill: kill}
}

func (b *budget) start() { b.resume() }

func (b *budget) resume() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running {
		return
	}
	b.running, b.since = true, time.Now()
	b.soft = time.AfterFunc(max(b.left, 0), b.interrupt)
	b.hard = time.AfterFunc(max(b.left, 0)+2*time.Second, b.kill)
}

func (b *budget) pause() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.running {
		return
	}
	b.running = false
	b.soft.Stop()
	b.hard.Stop()
	b.left -= time.Since(b.since)
}

func (b *budget) stop() { b.pause() }

// watchMemory stops the child when its heap passes limit. Allocated bytes include garbage
// not yet collected, so past the limit it collects once and measures again.
func watchMemory(limit int64, w *wire) {
	if limit <= 0 {
		return
	}
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	over := func() bool {
		metrics.Read(sample)
		return sample[0].Value.Kind() == metrics.KindUint64 && int64(sample[0].Value.Uint64()) > limit
	}
	for range time.Tick(20 * time.Millisecond) {
		if over() {
			runtime.GC()
			if over() {
				w.send(message{T: "fail", Text: fmt.Sprintf("the code used more than %d MiB of memory and was stopped", limit>>20)})
				os.Exit(exitMemory)
			}
		}
	}
}
