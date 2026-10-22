package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LaneBirmingham/shartifacts/internal/auth"
)

func init() { auth.Iterations = 1000 }

// The auth command turns sign-in on and off, and makes, lists and revokes tokens, in the
// data folder a running server reads.
func TestAuthCommand(t *testing.T) {
	data := t.TempDir()
	getenv := func(k string) string {
		if k == "SHARTIFACTS_DATA" {
			return data
		}
		return ""
	}
	run := func(input string, args ...string) (int, string, string) {
		t.Helper()
		in, err := os.Create(filepath.Join(t.TempDir(), "stdin"))
		if err != nil {
			t.Fatal(err)
		}
		in.WriteString(input)
		in.Seek(0, 0)
		defer in.Close()
		var out, errs bytes.Buffer
		code := authCommand(args, in, &out, &errs, getenv)
		return code, out.String(), errs.String()
	}

	if code, out, _ := run("", "status"); code != 0 || !strings.Contains(out, "Sign-in is off") {
		t.Fatalf("status: %d %q", code, out)
	}
	if code, _, errs := run("short\n", "set-password"); code != 1 || !strings.Contains(errs, "8") {
		t.Fatalf("a short password: %d %q", code, errs)
	}
	if code, _, errs := run("", "set-password"); code != 1 || !strings.Contains(errs, "no password") {
		t.Fatalf("no password: %d %q", code, errs)
	}
	if code, out, _ := run("correct horse battery\n", "set-password"); code != 0 || !strings.Contains(out, "Sign-in is on") {
		t.Fatalf("set-password: %d %q", code, out)
	}
	// A store opened afresh each time, as the server would see the file after its next check.
	open := func() *auth.Store {
		t.Helper()
		st, err := auth.Open(data)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	if !open().CheckPassword("correct horse battery") {
		t.Fatal("the piped password was not set")
	}

	code, token, _ := run("", "token", "Claude Code")
	token = strings.TrimSpace(token)
	if code != 0 || !strings.HasPrefix(token, auth.TokenPrefix) {
		t.Fatalf("token: %d %q", code, token)
	}
	if _, ok := open().Token(token); !ok {
		t.Fatal("the printed token does not work")
	}
	_, list, _ := run("", "tokens")
	id := open().Tokens()[0].ID
	if !strings.Contains(list, id) || !strings.Contains(list, "Claude Code") || strings.Contains(list, token) {
		t.Fatalf("tokens lists %q", list)
	}
	if code, _, _ := run("", "revoke", "nope"); code != 1 {
		t.Fatalf("revoking an unknown token: %d", code)
	}
	if code, _, _ := run("", "revoke", id); code != 0 {
		t.Fatalf("revoke: %d", code)
	}
	if _, ok := open().Token(token); ok {
		t.Fatal("a revoked token still works")
	}

	// A new password says which tokens still work, and revoke all ends them.
	run("", "token", "Codex")
	run("", "token", "Cursor")
	if _, out, _ := run("another good password\n", "set-password"); !strings.Contains(out, "2 agent tokens still work (Codex, Cursor)") {
		t.Fatalf("set-password says nothing of the tokens left: %q", out)
	}
	if code, out, _ := run("", "revoke", "all"); code != 0 || !strings.Contains(out, "Revoked 2") || len(open().Tokens()) != 0 {
		t.Fatalf("revoke all: %d %q", code, out)
	}

	code, out, _ := run("", "new-password")
	made := strings.TrimPrefix(strings.SplitN(out, "\n", 2)[0], "Password: ")
	if code != 0 || len(made) != 24 || !open().CheckPassword(made) {
		t.Fatalf("new-password: %d %q", code, out)
	}
	if code, _, _ := run("", "off"); code != 0 || open().Enabled() {
		t.Fatalf("off: %d", code)
	}

	if code, _, _ := run("", "bogus"); code != 2 {
		t.Fatalf("an unknown command: %d", code)
	}
	if code, _, _ := run("", "token"); code != 2 {
		t.Fatalf("token without a name: %d", code)
	}
}
