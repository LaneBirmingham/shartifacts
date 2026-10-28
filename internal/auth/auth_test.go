package auth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func init() { Iterations = 1000 }

func open(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func TestOffUntilAPasswordIsSet(t *testing.T) {
	s, _ := open(t)
	if s.Enabled() || s.CheckPassword("") || s.CheckPassword("anything at all") {
		t.Fatal("a fresh data directory has sign-in on")
	}
	if _, err := s.NewSession(); err == nil {
		t.Fatal("opened a session with sign-in off")
	}
	if err := s.SetPassword("short"); err != ErrPasswordLength {
		t.Fatalf("short password: %v", err)
	}
	if err := s.SetPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	if !s.Enabled() || !s.CheckPassword("correct horse") || s.CheckPassword("correct horsE") || s.CheckPassword("") {
		t.Fatal("password check")
	}
	if err := s.Disable(); err != nil {
		t.Fatal(err)
	}
	if s.Enabled() || s.CheckPassword("correct horse") {
		t.Fatal("still on after Disable")
	}
}

func TestSessions(t *testing.T) {
	s, _ := open(t)
	s.SetPassword("correct horse")
	a, err := s.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := s.NewSession()
	if !s.Session(a) || !s.Session(b) || s.Session("") || s.Session(a+"x") {
		t.Fatal("session check")
	}
	s.EndSession(a)
	if s.Session(a) || !s.Session(b) {
		t.Fatal("EndSession ended the wrong one")
	}
	// A new password signs everyone out.
	s.SetPassword("battery staple")
	if s.Session(b) {
		t.Fatal("a session survived a password change")
	}
	c, _ := s.NewSession()
	s.EndSessions()
	if s.Session(c) {
		t.Fatal("a session survived EndSessions")
	}
	// Sessions expire.
	d, _ := s.NewSession()
	s.now = func() time.Time { return time.Now().Add(SessionTTL + time.Minute) }
	if s.Session(d) {
		t.Fatal("an expired session is open")
	}
	// And turning sign-in off ends them all.
	s.now = time.Now
	e, _ := s.NewSession()
	s.Disable()
	s.SetPassword("battery staple")
	if s.Session(e) {
		t.Fatal("a session survived turning sign-in off and on")
	}
}

func TestTokens(t *testing.T) {
	s, _ := open(t)
	if _, _, err := s.NewToken("  "); err != ErrTokenName {
		t.Fatalf("blank name: %v", err)
	}
	v, tok, err := s.NewToken("Claude Code")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(v, TokenPrefix) || len(v) < 40 || !strings.HasPrefix(v, tok.Hint) {
		t.Fatalf("token %q, hint %q", v, tok.Hint)
	}
	got, ok := s.Token(v)
	if !ok || got.ID != tok.ID || got.LastUsedAt.IsZero() {
		t.Fatalf("Token = %+v, %v", got, ok)
	}
	for _, bad := range []string{"", v + "x", strings.TrimPrefix(v, TokenPrefix), TokenPrefix} {
		if _, ok := s.Token(bad); ok {
			t.Fatalf("accepted %q", bad)
		}
	}
	if list := s.Tokens(); len(list) != 1 || list[0].Name != "Claude Code" || !s.HasToken(tok.ID) {
		t.Fatalf("Tokens = %+v", list)
	}
	if err := s.RevokeToken(tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Token(v); ok || s.HasToken(tok.ID) {
		t.Fatal("a revoked token still works")
	}
	if err := s.RevokeToken(tok.ID); err != ErrNotFound {
		t.Fatalf("revoke twice: %v", err)
	}
}

// The CLI edits the file while the server runs; the server must notice.
func TestNoticesAnotherProcess(t *testing.T) {
	server, dir := open(t)
	if server.Enabled() {
		t.Fatal("on")
	}
	cli, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	v, _, _ := cli.NewToken("agent")
	cli.SetPassword("correct horse")
	server.checked = time.Time{}
	if !server.Enabled() || !server.CheckPassword("correct horse") {
		t.Fatal("the server missed a password set by the CLI")
	}
	if _, ok := server.Token(v); !ok {
		t.Fatal("the server missed a token made by the CLI")
	}
	// Edits from both sides merge rather than overwrite each other.
	session, _ := server.NewSession()
	cli.NewToken("second")
	server.checked = time.Time{}
	if !server.Session(session) || len(server.Tokens()) != 2 {
		t.Fatalf("lost an edit: session %v, %d tokens", server.Session(session), len(server.Tokens()))
	}
	// Recovery by deleting the file turns sign-in off.
	os.Remove(filepath.Join(dir, "meta", "auth.json"))
	server.checked = time.Time{}
	if server.Enabled() {
		t.Fatal("still on after the file was removed")
	}
}

func TestFileHoldsNoSecrets(t *testing.T) {
	s, dir := open(t)
	s.SetPassword("correct horse")
	session, _ := s.NewSession()
	v, _, _ := s.NewToken("agent")
	b, err := os.ReadFile(filepath.Join(dir, "meta", "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"correct horse", session, v, strings.TrimPrefix(v, TokenPrefix)} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("auth.json contains a secret: %s", b)
		}
	}
	fi, _ := os.Stat(filepath.Join(dir, "meta", "auth.json"))
	if fi.Mode().Perm() != 0600 {
		t.Fatalf("auth.json mode %v", fi.Mode().Perm())
	}
}

// A file the server cannot read keeps sign-in on and lets nothing in, rather than leaving
// the last state it read in force or opening the workspace.
func TestUnreadableFileFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any file")
	}
	s, dir := open(t)
	s.SetPassword("correct horse")
	session, _ := s.NewSession()
	token, _, _ := s.NewToken("agent")
	file := filepath.Join(dir, "meta", "auth.json")
	if err := os.Chmod(file, 0); err != nil {
		t.Fatal(err)
	}
	s.checked = time.Time{}
	if !s.Enabled() || s.Session(session) || s.CheckPassword("correct horse") {
		t.Fatal("an unreadable file still lets a session or the password in, or turned sign-in off")
	}
	if _, ok := s.Token(token); ok {
		t.Fatal("an unreadable file still lets a token in")
	}
	os.Chmod(file, 0600)
	s.checked = time.Time{}
	if !s.Session(session) || !s.CheckPassword("correct horse") {
		t.Fatal("did not recover once the file could be read again")
	}
}

// Two processes changing the file at once lose none of each other's edits.
func TestConcurrentWritersKeepEveryEdit(t *testing.T) {
	server, dir := open(t)
	server.SetPassword("correct horse")
	cli, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Contention is the point, but ErrLocked is the lock's documented answer to it: a caller
	// retries rather than the file corrupting. Forty writers at once on a slow pool (an fsync
	// each, tens of milliseconds on a FUSE-backed array) can outlast the lockWait acquisition
	// waits, so the test retries the way a caller would. What it holds to is the promise:
	// nothing is lost, and no lock is left behind.
	retry := func(do func() error) {
		for attempt := 0; ; attempt++ {
			err := do()
			if err == nil {
				return
			}
			if !errors.Is(err, ErrLocked) || attempt == 20 {
				t.Error(err)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			retry(func() error { _, err := server.NewSession(); return err })
		})
		wg.Go(func() {
			retry(func() error { _, _, err := cli.NewToken(fmt.Sprintf("agent %d", i)); return err })
		})
	}
	wg.Wait()
	server.checked = time.Time{}
	if st := server.Status(); st.Sessions != 20 || st.Tokens != 20 {
		t.Fatalf("lost edits: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(dir, "meta", "auth.json.lock")); !os.IsNotExist(err) {
		t.Fatalf("the lock file was left behind: %v", err)
	}
	// A lock left by a process that died is broken once it is stale.
	lock := filepath.Join(dir, "meta", "auth.json.lock")
	os.WriteFile(lock, nil, 0600)
	old := time.Now().Add(-2 * lockStale)
	os.Chtimes(lock, old, old)
	if _, _, err := cli.NewToken("after a crash"); err != nil {
		t.Fatalf("a stale lock: %v", err)
	}
}

// An edit that keeps the file's size and modification time is still noticed, as on
// filesystems that keep times only to the second.
func TestNoticesSameSizeEdit(t *testing.T) {
	server, dir := open(t)
	server.SetPassword("correct horse")
	a, _ := server.NewSession()
	file := filepath.Join(dir, "meta", "auth.json")
	fi, _ := os.Stat(file)
	b, _ := os.ReadFile(file)
	// Expire the session in place: same length, same time.
	expires := regexp.MustCompile(`"expires_at": "(\d{4})`)
	b = expires.ReplaceAll(b, []byte(`"expires_at": "2000`))
	os.WriteFile(file, b, 0600)
	os.Chtimes(file, fi.ModTime(), fi.ModTime())
	server.checked = time.Time{}
	if server.Session(a) {
		t.Fatal("missed an edit of the same size and time")
	}
}

func TestTurnOnOnlyOnce(t *testing.T) {
	s, _ := open(t)
	if err := s.TurnOn("correct horse"); err != nil {
		t.Fatal(err)
	}
	if err := s.TurnOn("battery staple"); err != ErrAlreadyOn || !s.CheckPassword("correct horse") {
		t.Fatalf("a second TurnOn: %v", err)
	}
}

func TestSessionIDs(t *testing.T) {
	s, _ := open(t)
	s.SetPassword("correct horse")
	v, _ := s.NewSession()
	id := SessionID(v)
	if strings.Contains(v, id) || !s.HasSession(id) || s.HasSession(SessionID("other")) || s.HasSession("") {
		t.Fatal("HasSession")
	}
	s.EndSession(v)
	if s.HasSession(id) {
		t.Fatal("an ended session is still there")
	}
	s.NewToken("a")
	s.NewToken("b")
	if n, err := s.RevokeTokens(); n != 2 || err != nil || len(s.Tokens()) != 0 {
		t.Fatalf("RevokeTokens: %d %v", n, err)
	}
}
