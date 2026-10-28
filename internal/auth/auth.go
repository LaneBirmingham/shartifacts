// Package auth is the workspace's own sign-in: one owner password, the browser sessions it
// opens, and the tokens agents send instead of a password. Sign-in is on exactly when a
// password is set, so a server nobody has set one on behaves as a trusted-network workspace,
// as it always has.
//
// Everything lives in meta/auth.json in the data directory. The `shartifacts auth` command
// edits the same file while the server runs (to set a password, or to recover from a lost
// one), so a running server rereads it when it changes. Secrets are never stored: the
// password as a salted PBKDF2 hash, sessions and tokens as SHA-256 hashes of random values.
package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Iterations is the PBKDF2-SHA-256 work factor for new password hashes, OWASP's figure for
// 2023. Each hash records its own count, so raising this leaves existing passwords valid.
// Tests lower it.
var Iterations = 600_000

const (
	// SessionTTL is how long a browser stays signed in.
	SessionTTL = 30 * 24 * time.Hour
	// TokenPrefix starts every agent token, so a leaked one is recognisable in logs and
	// secret scanners.
	TokenPrefix = "shx_"
	// MinPassword and MaxPassword bound a password's length, in characters and bytes.
	MinPassword = 8
	MaxPassword = 1024
	maxSessions = 100
	maxTokens   = 100
	// recheck is how often a running server looks for changes the CLI made to the file.
	recheck = time.Second
	// lastUsedEvery limits how often using a token rewrites the file.
	lastUsedEvery = time.Hour
)

var (
	ErrPasswordLength = fmt.Errorf("a password needs %d to %d characters", MinPassword, MaxPassword)
	ErrNotFound       = errors.New("no such token")
	ErrTokenName      = errors.New("a token name needs 1 to 60 characters")
	ErrTooMany        = fmt.Errorf("at most %d tokens; revoke some first", maxTokens)
	ErrAlreadyOn      = errors.New("a password is already set")
	ErrLocked         = errors.New("auth.json is being changed by another shartifacts; try again")
)

// Token is an agent token as listed: never its secret, which is shown once when made.
type Token struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Hint       string    `json:"hint"` // the secret's first characters, to tell tokens apart
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at,omitzero"`
}

// Status summarises the file for `shartifacts auth status` and the workspace.
type Status struct {
	Enabled       bool      `json:"enabled"`
	PasswordSetAt time.Time `json:"password_set_at,omitzero"`
	Sessions      int       `json:"sessions"`
	Tokens        int       `json:"tokens"`
}

type password struct {
	Algorithm  string    `json:"algorithm"`
	Iterations int       `json:"iterations"`
	Salt       []byte    `json:"salt"`
	Hash       []byte    `json:"hash"`
	SetAt      time.Time `json:"set_at"`
}

type session struct {
	Hash      string    `json:"hash"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type token struct {
	Token
	Hash string `json:"hash"`
}

type state struct {
	Password *password `json:"password,omitempty"`
	Sessions []session `json:"sessions"`
	Tokens   []token   `json:"tokens"`
}

// Store is the sign-in state of one data directory.
type Store struct {
	path string
	// now is the clock, replaced in tests.
	now func() time.Time

	mu      sync.Mutex
	st      state
	sum     [32]byte // the SHA-256 of the file st came from, to notice another process's writes
	checked time.Time
	// broken is why the file could not be read at the last check. Until it can be again,
	// sign-in stays on and nothing signs in: an unreadable file must not open the workspace.
	broken error
}

// Open loads dataDir's sign-in state; a directory without one has sign-in off.
func Open(dataDir string) (*Store, error) {
	s := &Store{path: filepath.Join(dataDir, "meta", "auth.json"), now: time.Now}
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return nil, err
	}
	if err := matchOwner(filepath.Dir(s.path), dataDir); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// load rereads the file if it changed since the last read. It compares the contents, not
// the modification time, which some filesystems keep only to the second. s.mu is held.
func (s *Store) load() error {
	s.checked = s.now()
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		s.st, s.sum, s.broken = state{}, [32]byte{}, nil
		return nil
	}
	if err == nil {
		sum := sha256.Sum256(b)
		if sum == s.sum && s.broken == nil {
			return nil
		}
		var st state
		if err = json.Unmarshal(b, &st); err == nil {
			s.st, s.sum, s.broken = st, sum, nil
			return nil
		}
	}
	s.broken = fmt.Errorf("read %s: %w (fix it, or delete it to turn sign-in off)", s.path, err)
	return s.broken
}

// fresh brings the state up to date for a read, checking the file at most once a second.
// s.mu is held.
func (s *Store) fresh() {
	if s.now().Sub(s.checked) < recheck {
		return
	}
	was := s.broken
	if err := s.load(); err != nil && (was == nil || was.Error() != err.Error()) {
		log.Printf("sign-in: %v; refusing every password, session and token until it can be read", err)
	}
}

// usable reports whether credentials can be checked: the file was readable last time.
// s.mu is held.
func (s *Store) usable() bool { return s.broken == nil }

// change applies edit to the latest state on disk and saves it, holding the lock file so
// that the server and the CLI never write over each other's edits. s.mu is held.
func (s *Store) change(edit func(*state) error) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.load(); err != nil {
		return err
	}
	st := s.st
	st.Sessions = slices.Clone(st.Sessions)
	st.Tokens = slices.Clone(st.Tokens)
	if err := edit(&st); err != nil {
		return err
	}
	now := s.now()
	st.Sessions = slices.DeleteFunc(st.Sessions, func(v session) bool { return !now.Before(v.ExpiresAt) })
	if st.Sessions == nil {
		st.Sessions = []session{}
	}
	if st.Tokens == nil {
		st.Tokens = []token{}
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err = writeAtomic(s.path, b); err != nil {
		return err
	}
	s.st, s.sum = st, sha256.Sum256(b)
	return nil
}

// lockStale is how old a lock file must be to count as left behind by a process that died
// holding it. Holding one takes milliseconds.
const lockStale = 10 * time.Second

// lockWait is how long lock waits for another process to let go. It is longer than lockStale
// so that a lock left by a process that died is broken rather than reported. Each hold
// syncs the file to disk, which on a slow or busy disk takes a good part of a second, and
// the other process may have a queue of edits to make first, so the wait is measured on
// the clock rather than by counting the sleeps between tries.
const lockWait = 2 * lockStale

// lock takes meta/auth.json.lock, waiting up to lockWait for another process to let go.
func (s *Store) lock() (func(), error) {
	name := s.path + ".lock"
	deadline := time.Now().Add(lockWait)
	for {
		f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			f.Close()
			return func() { os.Remove(name) }, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		if fi, err := os.Stat(name); err == nil && time.Since(fi.ModTime()) > lockStale {
			os.Remove(name)
			continue
		}
		if time.Now().After(deadline) {
			return nil, ErrLocked
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func writeAtomic(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".auth-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if err == nil {
		err = matchOwner(f.Name(), filepath.Dir(filepath.Dir(path)))
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// Enabled reports whether sign-in is on: whether a password is set, or the file that would
// say cannot be read.
func (s *Store) Enabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	return s.st.Password != nil || !s.usable()
}

// Status summarises the state.
func (s *Store) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	st := Status{Enabled: s.st.Password != nil || !s.usable(), Tokens: len(s.st.Tokens)}
	if s.st.Password != nil {
		st.PasswordSetAt = s.st.Password.SetAt
	}
	now := s.now()
	for _, v := range s.st.Sessions {
		if now.Before(v.ExpiresAt) {
			st.Sessions++
		}
	}
	return st
}

// ValidPassword reports whether pw may become the password.
func ValidPassword(pw string) error {
	if utf8.RuneCountInString(pw) < MinPassword || len(pw) > MaxPassword || !utf8.ValidString(pw) {
		return ErrPasswordLength
	}
	return nil
}

// SetPassword sets the owner password, which turns sign-in on, and signs every browser out.
// Agent tokens are separate credentials and keep working.
func (s *Store) SetPassword(pw string) error { return s.setPassword(pw, false) }

// TurnOn sets the first password, failing with ErrAlreadyOn if one was set meanwhile (by
// another browser, or by the CLI since this server last looked), so it never replaces one.
func (s *Store) TurnOn(pw string) error { return s.setPassword(pw, true) }

func (s *Store) setPassword(pw string, first bool) error {
	if err := ValidPassword(pw); err != nil {
		return err
	}
	salt := make([]byte, 16)
	rand.Read(salt)
	hash, err := pbkdf2.Key(sha256.New, pw, salt, Iterations, 32)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.change(func(st *state) error {
		if first && st.Password != nil {
			return ErrAlreadyOn
		}
		st.Password = &password{Algorithm: "pbkdf2-sha256", Iterations: Iterations, Salt: salt, Hash: hash, SetAt: s.now().UTC()}
		st.Sessions = nil
		return nil
	})
}

// CheckPassword reports whether pw is the owner password. With sign-in off nothing is.
func (s *Store) CheckPassword(pw string) bool {
	s.mu.Lock()
	s.fresh()
	p := s.st.Password
	if !s.usable() {
		p = nil
	}
	s.mu.Unlock()
	if p == nil || p.Algorithm != "pbkdf2-sha256" || p.Iterations < 1 || len(pw) > MaxPassword {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, p.Salt, p.Iterations, len(p.Hash))
	return err == nil && subtle.ConstantTimeCompare(got, p.Hash) == 1
}

// Disable removes the password, which turns sign-in off, and ends every session. Tokens
// are kept, so agents keep working if sign-in is turned back on.
func (s *Store) Disable() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.change(func(st *state) error {
		st.Password = nil
		st.Sessions = nil
		return nil
	})
}

func secret() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func digest(v string) string {
	h := sha256.Sum256([]byte(v))
	return hex.EncodeToString(h[:])
}

// equalDigest compares two hex digests in constant time.
func equalDigest(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// SessionID names the session with secret v, for things made on its behalf that should end
// with it. It is half the digest kept in the file, so it reveals nothing of the secret.
func SessionID(v string) string { return digest(v)[:32] }

// HasSession reports whether the session SessionID named is still open.
func (s *Store) HasSession(id string) bool {
	if len(id) != 32 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	if s.st.Password == nil || !s.usable() {
		return false
	}
	now := s.now()
	found := false
	for _, x := range s.st.Sessions {
		if equalDigest(x.Hash[:min(32, len(x.Hash))], id) && now.Before(x.ExpiresAt) {
			found = true
		}
	}
	return found
}

// NewSession opens a browser session and returns its secret, for the session cookie.
func (s *Store) NewSession() (string, error) {
	v := secret()
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.change(func(st *state) error {
		if st.Password == nil {
			return errors.New("sign-in is off")
		}
		now := s.now().UTC()
		st.Sessions = append(st.Sessions, session{Hash: digest(v), CreatedAt: now, ExpiresAt: now.Add(SessionTTL)})
		if len(st.Sessions) > maxSessions {
			st.Sessions = st.Sessions[len(st.Sessions)-maxSessions:]
		}
		return nil
	})
	return v, err
}

// Session reports whether v is the secret of an open session.
func (s *Store) Session(v string) bool {
	if v == "" {
		return false
	}
	d := digest(v)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	if s.st.Password == nil || !s.usable() {
		return false
	}
	now := s.now()
	found := false
	for _, x := range s.st.Sessions {
		if equalDigest(x.Hash, d) && now.Before(x.ExpiresAt) {
			found = true
		}
	}
	return found
}

// EndSession signs out the session with secret v.
func (s *Store) EndSession(v string) error {
	d := digest(v)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.change(func(st *state) error {
		st.Sessions = slices.DeleteFunc(st.Sessions, func(x session) bool { return equalDigest(x.Hash, d) })
		return nil
	})
}

// EndSessions signs every browser out.
func (s *Store) EndSessions() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.change(func(st *state) error {
		st.Sessions = nil
		return nil
	})
}

// NewToken makes an agent token and returns its secret, which is not kept and cannot be
// shown again.
func (s *Store) NewToken(name string) (string, Token, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 60 {
		return "", Token{}, ErrTokenName
	}
	v := TokenPrefix + secret()
	id := make([]byte, 8)
	rand.Read(id)
	t := Token{ID: hex.EncodeToString(id), Name: name, Hint: v[:len(TokenPrefix)+6], CreatedAt: s.now().UTC()}
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.change(func(st *state) error {
		if len(st.Tokens) >= maxTokens {
			return ErrTooMany
		}
		st.Tokens = append(st.Tokens, token{Token: t, Hash: digest(v)})
		return nil
	})
	return v, t, err
}

// Tokens lists the agent tokens, oldest first.
func (s *Store) Tokens() []Token {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	out := make([]Token, len(s.st.Tokens))
	for i, t := range s.st.Tokens {
		out[i] = t.Token
	}
	return out
}

// RevokeTokens deletes every token and returns how many there were.
func (s *Store) RevokeTokens() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	err := s.change(func(st *state) error {
		n = len(st.Tokens)
		st.Tokens = nil
		return nil
	})
	return n, err
}

// RevokeToken deletes a token; agents using it are refused from the next request.
func (s *Store) RevokeToken(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.change(func(st *state) error {
		n := len(st.Tokens)
		st.Tokens = slices.DeleteFunc(st.Tokens, func(t token) bool { return t.ID == id })
		if len(st.Tokens) == n {
			return ErrNotFound
		}
		return nil
	})
}

// Token reports which token v is the secret of, and notes that it was used.
func (s *Store) Token(v string) (Token, bool) {
	if !strings.HasPrefix(v, TokenPrefix) {
		return Token{}, false
	}
	d := digest(v)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	i := slices.IndexFunc(s.st.Tokens, func(t token) bool { return equalDigest(t.Hash, d) })
	if i < 0 || !s.usable() {
		return Token{}, false
	}
	t := s.st.Tokens[i].Token
	if now := s.now().UTC(); now.Sub(t.LastUsedAt) >= lastUsedEvery {
		// Best effort: a failed write must not refuse a valid token.
		s.change(func(st *state) error {
			for j := range st.Tokens {
				if st.Tokens[j].ID == t.ID {
					st.Tokens[j].LastUsedAt = now
				}
			}
			return nil
		})
		t.LastUsedAt = now
	}
	return t, true
}

// HasToken reports whether a token with this id exists.
func (s *Store) HasToken(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh()
	return s.usable() && slices.ContainsFunc(s.st.Tokens, func(t token) bool { return t.ID == id })
}
