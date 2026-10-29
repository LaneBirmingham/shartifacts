package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LaneBirmingham/shartifacts/internal/auth"
)

// Sign-in. With an owner password set (internal/auth), the workspace answers only browsers
// holding a session cookie and agents sending a token, on top of the network checks in
// guard, which stay. Share and app links are public by design and never ask.
//
// Some workspace URLs stay open without either, because what loads them cannot send a
// cookie: sandboxed previews and app pages run on an opaque origin, so their requests for
// libraries, media and files are cross-site, and browsers leave a SameSite cookie behind.
// Those URLs name things by unguessable random IDs (96 to 128 bits) that only signed-in
// people and agents can list, except media, which is named by the SHA-256 of its contents:
// someone who already has a file can learn whether the library holds it, and nothing more.
// An app page served without sign-in gets no app session, so its backend refuses it.

// sessionCookie holds a browser's session secret. It is host-only, HttpOnly and
// SameSite=Lax. Over HTTPS it is also Secure and named with the __Host- prefix, so a page
// on a sibling hostname (an app's, under the same domain) cannot set one that shadows it.
const (
	sessionCookie       = "shartifacts_session"
	secureSessionCookie = "__Host-" + sessionCookie
)

// uploadGrantTTL is how long an upload URL handed to an agent's shell keeps working.
const uploadGrantTTL = time.Hour

type credentialKey struct{}

// credential is what signed a request in: a browser session or an agent token.
type credential struct {
	session string
	token   auth.Token
}

func (s *Server) signInOn() bool { return s.Auth != nil && s.Auth.Enabled() }

// credentialOf checks a request's agent token, then its session cookies. Every cookie of
// either name is tried, so one set by someone else for a parent domain cannot hide the
// real one.
func (s *Server) credentialOf(r *http.Request) (credential, bool) {
	if scheme, v, ok := strings.Cut(r.Header.Get("Authorization"), " "); ok && strings.EqualFold(scheme, "Bearer") {
		if t, ok := s.Auth.Token(strings.TrimSpace(v)); ok {
			return credential{token: t}, true
		}
	}
	for _, name := range []string{secureSessionCookie, sessionCookie} {
		for _, c := range r.CookiesNamed(name) {
			if s.Auth.Session(c.Value) {
				return credential{session: c.Value}, true
			}
		}
	}
	return credential{}, false
}

// boundTo names the credential that signed r in, for what is made on its behalf and must
// end with it (app sessions, upload grants): "t:" and a token's id, or "s:" and a session's.
// It is empty while sign-in is off.
func (s *Server) boundTo(r *http.Request) string {
	c, ok := credentialFrom(r)
	switch {
	case !s.signInOn() || !ok:
		return ""
	case c.token.ID != "":
		return "t:" + c.token.ID
	default:
		return "s:" + auth.SessionID(c.session)
	}
}

// stillBound reports whether something made for a credential (boundTo) may still be used:
// always while sign-in is off, and otherwise only while that token or session exists. What
// was made while sign-in was off stops working when it is turned on.
func (s *Server) stillBound(binding string) bool {
	if !s.signInOn() {
		return true
	}
	kind, id, _ := strings.Cut(binding, ":")
	switch kind {
	case "t":
		return s.Auth.HasToken(id)
	case "s":
		return s.Auth.HasSession(id)
	}
	return false
}

// workspaceToken reports whether any of a request's Authorization headers carries a
// shartifacts agent token, valid or not.
func workspaceToken(h http.Header) bool {
	for _, v := range h.Values("Authorization") {
		if scheme, token, ok := strings.Cut(v, " "); ok && strings.EqualFold(scheme, "Bearer") && strings.HasPrefix(strings.TrimSpace(token), auth.TokenPrefix) {
			return true
		}
	}
	return false
}

func credentialFrom(r *http.Request) (credential, bool) {
	c, ok := r.Context().Value(credentialKey{}).(credential)
	return c, ok
}

// signedIn reports whether a workspace request may act as the owner: sign-in is off, or
// guard found a valid session or token on it, or, on an app's private hostname, that
// hostname's cookie.
func (s *Server) signedIn(r *http.Request) bool {
	if !s.signInOn() {
		return true
	}
	if v, ok := privateVisitOf(r); ok {
		return v.signed
	}
	_, ok := credentialFrom(r)
	return ok
}

// signInGate runs in guard for workspace requests once the network checks have passed. It
// returns the request with its credential, or false after refusing it.
func (s *Server) signInGate(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	if !s.signInOn() {
		return r, true
	}
	if c, ok := s.credentialOf(r); ok {
		return r.WithContext(context.WithValue(r.Context(), credentialKey{}, c)), true
	}
	if s.openWithoutSignIn(r) {
		return r, true
	}
	p := r.URL.Path
	if (r.Method == "GET" || r.Method == "HEAD") && workspacePage(p) != "" {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		return r, false
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="shartifacts"`)
	msg := "sign in first"
	if p == "/mcp" || r.Header.Get("Authorization") != "" {
		msg = "this shartifacts needs an agent token: make one with Connect an agent in the workspace (or `shartifacts auth token NAME` on the server) and send it as Authorization: Bearer <token>"
	}
	writeJSON(w, 401, map[string]string{"error": msg, "code": "unauthenticated"})
	return r, false
}

// openWithoutSignIn lists the workspace requests that need no session or token. Anything
// not named here is refused, so a route added later is private until someone decides
// otherwise.
func (s *Server) openWithoutSignIn(r *http.Request) bool {
	p := r.URL.Path
	read := r.Method == "GET" || r.Method == "HEAD"
	switch {
	case p == "/healthz", read && p == "/login", read && p == "/api/auth", r.Method == "POST" && p == "/api/auth/login":
		return true
	case read && strings.HasPrefix(p, "/.well-known/"):
		// Agents probe for OAuth metadata after a 401; let them find none rather than
		// another 401, so they report that a token is needed.
		return true
	case strings.HasPrefix(p, "/s/"):
		// Share links are public wherever they are opened, and carry their own token.
		return true
	case appPath(p):
		// serveApp gives an app page no app session without sign-in, and its backend
		// answers only signed-in requests or that session.
		return true
	case read && sandboxReadable(p):
		return true
	case r.Method == "POST" && strings.HasPrefix(p, "/api/media/"):
		// An agent's shell upload: the handler checks the grant in the URL.
		return true
	case read && s.uiFile(p):
		// The workspace's own scripts, styles and icons, which the login page needs too.
		return true
	}
	return false
}

// uiFile reports whether p is one of the embedded UI's static files, other than its pages.
func (s *Server) uiFile(p string) bool {
	name := strings.TrimPrefix(p, "/")
	if name == "" || strings.HasSuffix(name, ".html") || !fs.ValidPath(name) {
		return false
	}
	fi, err := fs.Stat(s.Web, name)
	return err == nil && !fi.IsDir()
}

func peerAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// passwordAttempts limits password tries. Requests reach it only from the LAN or tailnet,
// so this slows guessing rather than stopping a crowd. Every try counts from before its
// hash is computed, so tries sent at once cannot all slip under the limit, and a right
// password takes its own try back.
type passwordAttempts struct {
	mu    sync.Mutex
	tries map[string][]time.Time
	// hashing bounds how many password hashes run at once, whoever asks.
	hashing chan struct{}
}

const (
	attemptWindow = 15 * time.Minute
	// attemptLimit is per client. A reverse proxy in front of the workspace makes every
	// client arrive from its address, so clients are told apart by the address it adds to
	// X-Forwarded-For (the last one), and peerLimit caps the proxy's address as a whole,
	// since a client could write that header itself.
	attemptLimit  = 10
	peerLimit     = 100
	hashesAtOnce  = 2
	hashQueueWait = 10 * time.Second
)

// attemptKeys are the counters a password try from r counts against, with their limits.
func attemptKeys(r *http.Request) map[string]int {
	peer := peerAddr(r)
	client := peer
	if v := r.Header.Values("X-Forwarded-For"); len(v) > 0 {
		hops := strings.Split(v[len(v)-1], ",")
		if ip, ok := parseForwardedAddr(hops[len(hops)-1]); ok {
			client = peer + " for " + ip.String()
		}
	}
	return map[string]int{"client " + client: attemptLimit, "peer " + peer: peerLimit}
}

// begin counts a try against keys and returns zero, or how long to wait while any of them
// is at its limit, in which case nothing is counted.
func (a *passwordAttempts) begin(keys map[string]int, now time.Time) time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tries == nil {
		a.tries = map[string][]time.Time{}
	}
	if len(a.tries) > 10000 {
		for k := range a.tries {
			a.recent(k, now)
		}
	}
	var wait time.Duration
	for k, limit := range keys {
		if recent := a.recent(k, now); len(recent) >= limit {
			wait = max(wait, recent[len(recent)-limit].Add(attemptWindow).Sub(now))
		}
	}
	if wait > 0 {
		return wait
	}
	for k := range keys {
		a.tries[k] = append(a.tries[k], now)
	}
	return 0
}

// recent drops tries older than the window. a.mu is held.
func (a *passwordAttempts) recent(key string, now time.Time) []time.Time {
	list := a.tries[key]
	for len(list) > 0 && now.Sub(list[0]) >= attemptWindow {
		list = list[1:]
	}
	if len(list) == 0 {
		delete(a.tries, key)
	} else {
		a.tries[key] = list
	}
	return list
}

// succeeded takes back the try begin counted at now, and clears the client's wrong ones.
func (a *passwordAttempts) succeeded(keys map[string]int, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, limit := range keys {
		if limit == attemptLimit {
			delete(a.tries, k)
			continue
		}
		list := a.tries[k]
		if i := slices.Index(list, now); i >= 0 {
			a.tries[k] = slices.Delete(list, i, i+1)
		}
	}
}

// hash waits for a turn to compute a password hash, and returns the function that ends it.
func (a *passwordAttempts) hash(ctx context.Context) (func(), bool) {
	a.mu.Lock()
	if a.hashing == nil {
		a.hashing = make(chan struct{}, hashesAtOnce)
	}
	slots := a.hashing
	a.mu.Unlock()
	t := time.NewTimer(hashQueueWait)
	defer t.Stop()
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, true
	case <-t.C:
	case <-ctx.Done():
	}
	return nil, false
}

// checkPassword checks the owner password, refusing a client that got it wrong too often.
// It has answered the request when it returns false.
func (s *Server) checkPassword(w http.ResponseWriter, r *http.Request, pw string) bool {
	keys, now := attemptKeys(r), time.Now()
	if wait := s.attempts.begin(keys, now); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeJSON(w, 429, map[string]string{"error": fmt.Sprintf("too many wrong passwords from this address; try again in %d minutes", int(wait.Minutes())+1), "code": "rate_limited"})
		return false
	}
	done, ok := s.attempts.hash(r.Context())
	if !ok {
		w.Header().Set("Retry-After", "10")
		writeJSON(w, 503, map[string]string{"error": "too many sign-ins at once; try again shortly", "code": "busy"})
		return false
	}
	right := s.Auth.CheckPassword(pw)
	done()
	if !right {
		writeJSON(w, 403, map[string]string{"error": "wrong password", "code": "wrong_password"})
		return false
	}
	s.attempts.succeeded(keys, now)
	return true
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	secure := s.clientScheme(r) == "https"
	name := sessionCookie
	if secure {
		name = secureSessionCookie
	}
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge, HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode})
}

// startSession signs this browser in.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request) bool {
	v, err := s.Auth.NewSession()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "cannot sign in: " + err.Error()})
		return false
	}
	s.setSessionCookie(w, r, v, int(auth.SessionTTL.Seconds()))
	return true
}

// fromBrowser answers the request and returns false unless sign-in is on and a browser
// session (not an agent token) signed it in: only people manage passwords and tokens.
func (s *Server) fromBrowser(w http.ResponseWriter, r *http.Request) (credential, bool) {
	if !s.signInOn() {
		writeJSON(w, 409, map[string]string{"error": "sign-in is off: set a password first", "code": "sign_in_off"})
		return credential{}, false
	}
	c, ok := credentialFrom(r)
	if !ok || c.session == "" {
		writeJSON(w, 403, map[string]string{"error": "only a signed-in browser can do this, not an agent token"})
		return credential{}, false
	}
	return c, true
}

// safeNext keeps a post-sign-in redirect on this server: a path, in printable ASCII with no
// backslash. Browsers drop tabs and newlines from URLs and read a backslash as a slash, so
// anything else could turn "/" plus something into "//elsewhere".
func safeNext(next string) string {
	u, err := url.Parse(next)
	bad := strings.ContainsFunc(next, func(c rune) bool { return c <= ' ' || c > '~' || c == '\\' })
	if err != nil || bad || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return next
}

func (s *Server) authRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		if !s.signInOn() {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		if _, ok := credentialFrom(r); ok {
			http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
			return
		}
		s.writePage(w, "login.html")
	})
	mux.HandleFunc("GET /api/auth", func(w http.ResponseWriter, r *http.Request) {
		c, signed := credentialFrom(r)
		out := map[string]any{"enabled": s.signInOn(), "signed_in": signed, "via": ""}
		if signed {
			out["via"] = "session"
			if c.token.ID != "" {
				out["via"] = "token"
			}
		}
		writeJSON(w, 200, out)
	})
	mux.HandleFunc("POST /api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Password string `json:"password"`
		}
		if !decode(w, r, &in) {
			return
		}
		if !s.signInOn() {
			writeJSON(w, 409, map[string]string{"error": "sign-in is off, so there is no password to check", "code": "sign_in_off"})
			return
		}
		if s.checkPassword(w, r, in.Password) && s.startSession(w, r) {
			writeJSON(w, 200, map[string]bool{"signed_in": true})
		}
	})
	mux.HandleFunc("POST /api/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Everywhere bool `json:"everywhere"`
		}
		if r.ContentLength != 0 && !decode(w, r, &in) {
			return
		}
		if c, ok := credentialFrom(r); ok {
			var err error
			switch {
			case in.Everywhere && c.session == "":
				writeJSON(w, 403, map[string]string{"error": "only a signed-in browser can sign everyone out"})
				return
			case in.Everywhere:
				err = s.Auth.EndSessions()
			case c.session != "":
				err = s.Auth.EndSession(c.session)
			}
			if err != nil {
				writeJSON(w, 500, map[string]string{"error": err.Error()})
				return
			}
		}
		s.setSessionCookie(w, r, "", -1)
		writeJSON(w, 200, map[string]bool{"signed_in": false})
	})
	// Setting the first password needs only what the workspace needs without sign-in:
	// whoever can use it can lock it. Changing or removing one needs the current password
	// from a signed-in browser.
	mux.HandleFunc("PUT /api/auth/password", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Current      string `json:"current"`
			Password     string `json:"password"`
			RevokeTokens bool   `json:"revoke_tokens"`
		}
		if !decode(w, r, &in) {
			return
		}
		if err := auth.ValidPassword(in.Password); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		set := s.Auth.TurnOn
		if s.signInOn() {
			if _, ok := s.fromBrowser(w, r); !ok || !s.checkPassword(w, r, in.Current) {
				return
			}
			set = s.Auth.SetPassword
		}
		if err := set(in.Password); errors.Is(err, auth.ErrAlreadyOn) {
			writeJSON(w, 409, map[string]string{"error": "a password was set meanwhile: sign in with it", "code": "sign_in_on"})
			return
		} else if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		// Changing the password because someone else may know it calls for this too: a
		// token they made keeps working otherwise.
		if in.RevokeTokens {
			if _, err := s.Auth.RevokeTokens(); err != nil {
				writeJSON(w, 500, map[string]string{"error": err.Error()})
				return
			}
		}
		// Every other browser is signed out; this one stays in.
		if s.startSession(w, r) {
			writeJSON(w, 200, map[string]bool{"enabled": true})
		}
	})
	mux.HandleFunc("DELETE /api/auth/password", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Current string `json:"current"`
		}
		if !decode(w, r, &in) {
			return
		}
		if _, ok := s.fromBrowser(w, r); !ok || !s.checkPassword(w, r, in.Current) {
			return
		}
		if err := s.Auth.Disable(); err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		s.setSessionCookie(w, r, "", -1)
		writeJSON(w, 200, map[string]bool{"enabled": false})
	})
	mux.HandleFunc("GET /api/auth/tokens", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.fromBrowser(w, r); ok {
			writeJSON(w, 200, map[string]any{"tokens": s.Auth.Tokens()})
		}
	})
	mux.HandleFunc("POST /api/auth/tokens", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name string `json:"name"`
		}
		if !decode(w, r, &in) {
			return
		}
		if _, ok := s.fromBrowser(w, r); !ok {
			return
		}
		v, t, err := s.Auth.NewToken(in.Name)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 201, map[string]any{"token": v, "id": t.ID, "name": t.Name, "hint": t.Hint, "created_at": t.CreatedAt})
	})
	mux.HandleFunc("DELETE /api/auth/tokens/{id}", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.fromBrowser(w, r); !ok {
			return
		}
		if err := s.Auth.RevokeToken(r.PathValue("id")); err != nil {
			writeJSON(w, 404, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]bool{"revoked": true})
	})
	mux.HandleFunc("POST /api/media/{grant}", func(w http.ResponseWriter, r *http.Request) {
		if !s.validUploadGrant(r.PathValue("grant")) {
			writeJSON(w, 403, map[string]string{"error": "this upload URL has expired or its token was revoked: call list_media for a fresh one", "code": "unauthenticated"})
			return
		}
		s.uploadMedia(w, r)
	})
}

// An upload grant lets an agent's shell upload media with curl, which never sees the
// agent's token: MCP hands out upload URLs carrying one (uploadPath). It is good for an hour
// and only while the token or session that asked for it exists (boundTo).
type uploadGrant struct {
	Expires int64  `json:"e"`
	Bind    string `json:"b"`
}

func (s *Server) grantKey() []byte {
	s.grantKeyOnce.Do(func() {
		k, err := s.Store.Secret("upload-grant")
		if err != nil {
			panic(fmt.Sprintf("upload grant key: %v", err))
		}
		s.grantKeyBytes = k
	})
	return s.grantKeyBytes
}

func (s *Server) signGrant(g uploadGrant) string {
	b, _ := json.Marshal(g)
	payload := base64.RawURLEncoding.EncodeToString(b)
	mac := hmac.New(sha256.New, s.grantKey())
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) validUploadGrant(v string) bool {
	payload, sig, ok := strings.Cut(v, ".")
	if !ok {
		return false
	}
	mac := hmac.New(sha256.New, s.grantKey())
	mac.Write([]byte(payload))
	if !hmac.Equal([]byte(sig), []byte(base64.RawURLEncoding.EncodeToString(mac.Sum(nil)))) {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(payload)
	var g uploadGrant
	if err != nil || json.Unmarshal(b, &g) != nil || time.Now().Unix() > g.Expires {
		return false
	}
	return s.stillBound(g.Bind)
}

// uploadPath is the media upload path for an MCP request's shell examples: with sign-in on,
// one carrying a grant tied to the request's token or session.
func (s *Server) uploadPath(r *http.Request) string {
	if !s.signInOn() {
		return "/api/media"
	}
	return "/api/media/" + s.signGrant(uploadGrant{Expires: time.Now().Add(uploadGrantTTL).Unix(), Bind: s.boundTo(r)})
}
