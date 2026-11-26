package codemode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
)

// The test binary stands in for shartifacts: re-executed with ChildArg, it is the child.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == ChildArg {
		Child()
	}
	os.Exit(m.Run())
}

var runner = &Runner{}

func runJob(t *testing.T, job Job, call Call) *Result {
	t.Helper()
	if call == nil {
		call = func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
			t.Fatal("unexpected call")
			return nil, nil
		}
	}
	res, err := runner.Run(context.Background(), job, call)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestShapesOfCode(t *testing.T) {
	for _, tc := range []struct{ name, code, want string }{
		{"async arrow", "async () => { return {a: 1} }", `{"a":1}`},
		{"trailing semicolon", "async () => 2;", `2`},
		{"plain function", "function main() { return [1, 2] }", `[1,2]`},
		{"statements", "const x = 2;\nreturn x * 3", `6`},
		{"expression", "40 + 2", `42`},
		{"typescript", "async (): Promise<number> => { const n: number = 5; return n }", `5`},
		{"typescript statements", "interface P { x: number }\nconst p: P = {x: 7}\nreturn p.x", `7`},
		{"modern syntax", "async () => { const o = {a: {b: null}}; return {x: o.a?.b ?? 'none', g: Object.groupBy([1, 2, 3], n => n % 2 ? 'odd' : 'even')} }", `{"x":"none","g":{"odd":[1,3],"even":[2]}}`},
		{"string", "async () => 'hello'", `"hello"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := runJob(t, Job{Code: tc.code}, nil)
			if res.Error != "" || string(res.Value) != tc.want {
				t.Fatalf("got %s (error %q), want %s", res.Value, res.Error, tc.want)
			}
		})
	}
	if res := runJob(t, Job{Code: "async () => { console.log('x') }"}, nil); res.Error != "" || res.Value != nil {
		t.Fatalf("undefined should return no value: %+v", res)
	}
}

func TestErrorsSayWhereAndWhy(t *testing.T) {
	res := runJob(t, Job{Code: "async () => {\n  const a = 1\n  throw new Error('bad thing')\n}"}, nil)
	if !strings.Contains(res.Error, "bad thing") || !strings.Contains(res.Error, "code:3") {
		t.Fatalf("want the message and line 3: %q", res.Error)
	}
	res = runJob(t, Job{Code: "async () => { return ( }"}, nil)
	if !strings.HasPrefix(res.Error, "SyntaxError") {
		t.Fatalf("want a syntax error: %q", res.Error)
	}
	res = runJob(t, Job{Code: "async () => { throw {code: 7} }"}, nil)
	if !strings.Contains(res.Error, `{"code":7}`) {
		t.Fatalf("a thrown object should be shown: %q", res.Error)
	}
	res = runJob(t, Job{Code: "async () => { const f = () => f(); f() }"}, nil)
	if !strings.Contains(res.Error, "call stack") {
		t.Fatalf("runaway recursion: %q", res.Error)
	}
	res = runJob(t, Job{Code: "async () => { await new Promise(() => {}) }"}, nil)
	if !strings.Contains(res.Error, "never settled") {
		t.Fatalf("a promise that cannot settle: %q", res.Error)
	}
}

func TestOperations(t *testing.T) {
	var calls []string
	call := func(ctx context.Context, op string, args json.RawMessage) (json.RawMessage, error) {
		calls = append(calls, op+" "+string(args))
		switch op {
		case "get":
			var in struct{ ID string }
			json.Unmarshal(args, &in)
			return json.Marshal(map[string]any{"id": in.ID, "version": 3})
		case "fail":
			return nil, errors.New("version conflict")
		}
		return json.RawMessage(`"text result"`), nil
	}
	job := Job{API: "shartifacts", Ops: []string{"get", "fail", "text"}, Code: `async () => {
		const a = await shartifacts.get({id: "x1"})
		const b = await shartifacts.get({id: "x" + (a.version + 1)})
		let caught = ""
		try { await shartifacts.fail({}) } catch (e) { caught = e.message + " in " + e.operation }
		const t = await shartifacts.text()
		const both = await Promise.all([shartifacts.get({id: "p"}), shartifacts.get({id: "q"})])
		return {b: b.id, caught, t, both: both.map(x => x.id)}
	}`}
	res := runJob(t, job, call)
	if res.Error != "" || string(res.Value) != `{"b":"x4","caught":"version conflict in fail","t":"text result","both":["p","q"]}` {
		t.Fatalf("%s %q", res.Value, res.Error)
	}
	if len(calls) != 6 || calls[0] != `get {"id":"x1"}` || calls[3] != "text {}" {
		t.Fatal(calls)
	}
	if len(res.Calls) != 6 || res.Calls[2].Error != "version conflict" {
		t.Fatal(res.Calls)
	}

	res = runJob(t, Job{API: "shartifacts", Ops: []string{"get"}, Code: "async () => shartifacts.gte({})"}, call)
	if !strings.Contains(res.Error, `no operation "gte"`) || !strings.Contains(res.Error, "it has get") {
		t.Fatalf("a misspelt operation should list the real ones: %q", res.Error)
	}
	res = runJob(t, Job{API: "shartifacts", Ops: []string{"get"}, Code: "async () => shartifacts.get('x1')"}, call)
	if !strings.Contains(res.Error, "takes one object") {
		t.Fatalf("a bare argument should be refused: %q", res.Error)
	}
	res = runJob(t, Job{API: "shartifacts", Ops: []string{"fail"}, Code: "async () => { await shartifacts.fail({}) }"}, call)
	if !strings.Contains(res.Error, "version conflict") {
		t.Fatalf("an uncaught failure should surface: %q", res.Error)
	}
}

func TestGlobalsAndConsole(t *testing.T) {
	spec := json.RawMessage(`{"operations":{"a":{"description":"first"},"b":{"description":"second"}}}`)
	res := runJob(t, Job{Globals: map[string]json.RawMessage{"spec": spec}, Code: `async () => {
		console.log("keys", Object.keys(spec.operations))
		console.warn({n: 1})
		console.error(new Error("shown"))
		return Object.entries(spec.operations).filter(([k, v]) => v.description.startsWith("s")).map(([k]) => k)
	}`}, nil)
	if res.Error != "" || string(res.Value) != `["b"]` {
		t.Fatalf("%s %q", res.Value, res.Error)
	}
	if len(res.Logs) != 3 || res.Logs[0] != `keys ["a","b"]` || res.Logs[1] != `warn: {"n":1}` || !strings.HasPrefix(res.Logs[2], "error: Error: shown") {
		t.Fatalf("%q", res.Logs)
	}
}

// The sandbox has nothing to reach the machine with: no Node or browser APIs, no files, no
// network, and an empty environment.
func TestNothingToReachOut(t *testing.T) {
	res := runJob(t, Job{Code: `async () => [typeof require, typeof process, typeof fetch, typeof XMLHttpRequest, typeof Deno, typeof Bun, typeof WebSocket, typeof importScripts]`}, nil)
	if string(res.Value) != `["undefined","undefined","undefined","undefined","undefined","undefined","undefined","undefined"]` {
		t.Fatalf("%s %q", res.Value, res.Error)
	}
	// A source map comment naming a file is not read.
	res = runJob(t, Job{Code: "async () => { throw new Error('x') }\n//# sourceMappingURL=/etc/passwd"}, nil)
	if !strings.Contains(res.Error, "Error: x") {
		t.Fatalf("%q", res.Error)
	}
}

func TestTimersAndBase64(t *testing.T) {
	start := time.Now()
	res := runJob(t, Job{Limits: Limits{CPU: 200 * time.Millisecond}, Code: `async () => {
		const order = []
		const id = setTimeout(() => order.push("cleared"), 10)
		clearTimeout(id)
		setTimeout(() => order.push("late"), 40)
		setTimeout(v => order.push(v), 20, "early")
		await new Promise(r => setTimeout(r, 300))
		return {order, b: btoa("hi"), a: atob("aGk=")}
	}`}, nil)
	if res.Error != "" || string(res.Value) != `{"order":["early","late"],"b":"aGk=","a":"hi"}` {
		t.Fatalf("%s %q", res.Value, res.Error)
	}
	if time.Since(start) < 300*time.Millisecond {
		t.Fatal("the timer did not wait")
	}
}

func TestLimits(t *testing.T) {
	res := runJob(t, Job{Limits: Limits{CPU: 300 * time.Millisecond}, Code: "async () => { while (true) {} }"}, nil)
	if !strings.Contains(res.Error, "more than 300ms") {
		t.Fatalf("an endless loop should be stopped: %q", res.Error)
	}
	// Serialising the result runs the code's toJSON, which counts too.
	res = runJob(t, Job{Limits: Limits{CPU: 300 * time.Millisecond, Wall: 10 * time.Second}, Code: "async () => ({toJSON() { while (true) {} }})"}, nil)
	if !strings.Contains(res.Error, "more than 300ms") {
		t.Fatalf("an endless toJSON should be stopped: %q", res.Error)
	}
	// Waiting on operations is not JavaScript time.
	slow := func(ctx context.Context, op string, args json.RawMessage) (json.RawMessage, error) {
		time.Sleep(150 * time.Millisecond)
		return json.RawMessage(`true`), nil
	}
	res = runJob(t, Job{Limits: Limits{CPU: 200 * time.Millisecond}, API: "api", Ops: []string{"wait"}, Code: "async () => { for (let i = 0; i < 3; i++) await api.wait({}); return 'done' }"}, slow)
	if res.Error != "" || string(res.Value) != `"done"` {
		t.Fatalf("time spent in operations counted against the budget: %q", res.Error)
	}
	res = runJob(t, Job{Limits: Limits{Memory: 64 << 20}, Code: "async () => { const a = []; while (true) a.push('x'.repeat(1 << 16) + a.length) }"}, nil)
	if !strings.Contains(res.Error, "memory") {
		t.Fatalf("runaway allocation should be stopped: %q", res.Error)
	}
	res = runJob(t, Job{Limits: Limits{Memory: 64 << 20, CPU: 10 * time.Second}, Code: "async () => { const b = new ArrayBuffer(1 << 30); while (b) {} }"}, nil)
	if !strings.Contains(res.Error, "memory") {
		t.Fatalf("one huge allocation should be stopped: %q", res.Error)
	}
	ok := func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`1`), nil
	}
	res = runJob(t, Job{Limits: Limits{Calls: 3}, API: "api", Ops: []string{"op"}, Code: "async () => { for (let i = 0; i < 5; i++) await api.op({}) }"}, ok)
	if !strings.Contains(res.Error, "made its 3 calls") || len(res.Calls) != 4 {
		t.Fatalf("the call limit: %q %v", res.Error, res.Calls)
	}
	res = runJob(t, Job{Limits: Limits{Wall: 300 * time.Millisecond}, Code: "async () => { await new Promise(r => setTimeout(r, 5000)) }"}, nil)
	if !strings.Contains(res.Error, "longer than 300ms") {
		t.Fatalf("the wall limit: %q", res.Error)
	}
	// After all that, the sandbox is still there for the next run.
	if res := runJob(t, Job{Code: "async () => 1"}, nil); string(res.Value) != "1" {
		t.Fatal(res)
	}
}

func TestCancelStopsTheRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := runner.Run(ctx, Job{Code: "async () => { await new Promise(r => setTimeout(r, 5000)) }"}, nil)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
		t.Fatal(err, time.Since(start))
	}
}

func TestSignature(t *testing.T) {
	type Edit struct {
		Old string `json:"old_str"`
		New string `json:"new_str"`
	}
	type Input struct {
		ID      string             `json:"id"`
		Content *string            `json:"content,omitempty" jsonschema:"Full new source"`
		Edits   []Edit             `json:"edits,omitempty"`
		Files   map[string]*string `json:"files,omitempty"`
		Extra   map[string]any     `json:"extra,omitempty"`
		Limit   int                `json:"limit,omitempty"`
		Tags    *[]string          `json:"tags,omitempty"`
	}
	schema, err := jsonschema.For[Input](&jsonschema.ForOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := Signature("update", "Change it.", schema)
	want := `/** Change it. */
update(input: {
  id: string;
  /** Full new source */
  content?: string;
  edits?: {
    old_str: string;
    new_str: string;
  }[];
  files?: { [key: string]: null | string };
  extra?: { [key: string]: any };
  limit?: number;
  tags?: string[];
}): Promise<any>`
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	empty, _ := jsonschema.For[struct{}](&jsonschema.ForOptions{})
	if got := Signature("design", "", empty); got != "design(input?: {}): Promise<any>" {
		t.Fatal(got)
	}
}

func TestConsoleIsBounded(t *testing.T) {
	res := runJob(t, Job{Code: `async () => { console.log("é".repeat(40000)); for (let i = 0; i < 100; i++) console.log("more") }`}, nil)
	total := 0
	for _, l := range res.Logs {
		total += len(l)
	}
	if res.Error != "" || total > maxLogs+200 || !strings.HasSuffix(res.Logs[0], "…") || !strings.Contains(res.Logs[len(res.Logs)-1], "dropped") {
		t.Fatalf("%d bytes in %d lines, error %q", total, len(res.Logs), res.Error)
	}
	if !utf8.ValidString(res.Logs[0]) {
		t.Fatal("a cut line should stay valid UTF-8")
	}
}
