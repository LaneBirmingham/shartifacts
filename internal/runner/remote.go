package runner

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The runner sidecar's API. Every request carries the shared token in TokenHeader: the
// SHARTIFACTS_RUNNER_TOKEN environment variable when set, otherwise the .runner-token file
// shartifacts writes into the apps directory both containers mount. App backends cannot
// read that file through Landlock (they see only their build and data).
const TokenHeader = "X-Shartifacts-Runner-Token"

func tokenFile(dir string) string { return filepath.Join(dir, ".runner-token") }

// EnsureToken returns the shared token, creating the token file when there is none.
func EnsureToken(dir string) (string, error) {
	if t := os.Getenv("SHARTIFACTS_RUNNER_TOKEN"); t != "" {
		return t, nil
	}
	if b, err := os.ReadFile(tokenFile(dir)); err == nil && len(bytes.TrimSpace(b)) >= 32 {
		return string(bytes.TrimSpace(b)), nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	t := hex.EncodeToString(b)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	return t, os.WriteFile(tokenFile(dir), []byte(t+"\n"), 0600)
}

// Handler serves the runner API for a sidecar.
func Handler(l *Local) http.Handler {
	var mu sync.Mutex
	var token string
	current := func() string {
		if t := os.Getenv("SHARTIFACTS_RUNNER_TOKEN"); t != "" {
			return t
		}
		mu.Lock()
		defer mu.Unlock()
		if token == "" {
			if b, err := os.ReadFile(tokenFile(l.Dir)); err == nil {
				token = string(bytes.TrimSpace(b))
			}
		}
		return token
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /info", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, l.Info(r.Context())) })
	mux.HandleFunc("POST /build", func(w http.ResponseWriter, r *http.Request) {
		var req BuildRequest
		if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req) != nil {
			writeError(w, 400, "invalid build request")
			return
		}
		writeJSON(w, l.Build(r.Context(), req))
	})
	type target struct {
		App     string `json:"app"`
		Version int    `json:"version"`
	}
	mux.HandleFunc("POST /start", func(w http.ResponseWriter, r *http.Request) {
		var t target
		if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&t) != nil {
			writeError(w, 400, "invalid request")
			return
		}
		if err := l.Start(r.Context(), t.App, t.Version); err != nil {
			writeError(w, 502, err.Error())
			return
		}
		writeJSON(w, map[string]bool{"started": true})
	})
	mux.HandleFunc("POST /stop", func(w http.ResponseWriter, r *http.Request) {
		var t target
		if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&t) != nil {
			writeError(w, 400, "invalid request")
			return
		}
		if err := l.Stop(r.Context(), t.App, t.Version); err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, map[string]bool{"stopped": true})
	})
	mux.HandleFunc("GET /logs", func(w http.ResponseWriter, r *http.Request) {
		v, _ := strconv.Atoi(r.URL.Query().Get("version"))
		tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))
		lg, err := l.Logs(r.Context(), r.URL.Query().Get("app"), v, tail)
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		writeJSON(w, lg)
	})
	mux.HandleFunc("/proxy/{app}/{version}/{path...}", func(w http.ResponseWriter, r *http.Request) {
		v, err := strconv.Atoi(r.PathValue("version"))
		if err != nil {
			writeError(w, 400, "invalid version")
			return
		}
		r.URL.Path = "/" + r.PathValue("path")
		r.URL.RawPath = ""
		l.Proxy(w, r, r.PathValue("app"), v)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]string{"status": "ok"}) })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			want := current()
			got := r.Header.Get(TokenHeader)
			if want == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
				writeError(w, 401, "runner token missing or wrong")
				return
			}
		}
		r.Header.Del(TokenHeader)
		mux.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// Remote is a runner sidecar, reached over HTTP.
type Remote struct {
	URL   *url.URL
	Token string

	once   sync.Once
	client *http.Client
	proxy  *httputil.ReverseProxy
	// info is cached briefly: every open hub polls it.
	infoMu sync.Mutex
	info   Info
	infoAt time.Time
}

func (c *Remote) init() {
	c.once.Do(func() {
		c.client = &http.Client{}
		c.proxy = &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.Out.URL.Scheme, pr.Out.URL.Host = c.URL.Scheme, c.URL.Host
				pr.Out.Host = pr.In.Host
				pr.Out.Header.Set(TokenHeader, c.Token)
				keepForwarded(pr)
			},
			FlushInterval: -1,
			ModifyResponse: func(res *http.Response) error {
				for _, h := range []string{"Access-Control-Allow-Origin", "Access-Control-Allow-Credentials", "Access-Control-Expose-Headers"} {
					res.Header.Del(h)
				}
				filterCookies(res)
				return nil
			},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				if r.Context().Err() == nil {
					writeError(w, http.StatusBadGateway, "app runner unreachable: "+err.Error())
				}
			},
		}
	})
}

func (c *Remote) call(ctx context.Context, method, path string, in, out any, timeout time.Duration) error {
	c.init()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.URL.String(), "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set(TokenHeader, c.Token)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("app runner unreachable: %w", err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if res.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("app runner: %s", res.Status)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func (c *Remote) Info(ctx context.Context) Info {
	c.infoMu.Lock()
	defer c.infoMu.Unlock()
	if time.Since(c.infoAt) < 10*time.Second {
		return c.info
	}
	var info Info
	if err := c.call(ctx, "GET", "/info", nil, &info, 5*time.Second); err != nil {
		// Not cached, so the hub notices as soon as the runner is back.
		return Info{Remote: true, Error: err.Error()}
	}
	info.Remote = true
	c.info, c.infoAt = info, time.Now()
	return info
}

func (c *Remote) Build(ctx context.Context, req BuildRequest) BuildResult {
	var res BuildResult
	if err := c.call(ctx, "POST", "/build", req, &res, BuildTimeout+time.Minute); err != nil {
		return BuildResult{Error: err.Error()}
	}
	return res
}

func (c *Remote) Start(ctx context.Context, app string, version int) error {
	return c.call(ctx, "POST", "/start", map[string]any{"app": app, "version": version}, nil, time.Minute)
}

func (c *Remote) Stop(ctx context.Context, app string, version int) error {
	return c.call(ctx, "POST", "/stop", map[string]any{"app": app, "version": version}, nil, 30*time.Second)
}

func (c *Remote) Logs(ctx context.Context, app string, version int, tail int) (Logs, error) {
	var lg Logs
	err := c.call(ctx, "GET", fmt.Sprintf("/logs?app=%s&version=%d&tail=%d", url.QueryEscape(app), version, tail), nil, &lg, 10*time.Second)
	return lg, err
}

func (c *Remote) Proxy(w http.ResponseWriter, r *http.Request, app string, version int) {
	c.init()
	r.URL.Path = fmt.Sprintf("/proxy/%s/%d%s", app, version, r.URL.Path)
	r.URL.RawPath = ""
	c.proxy.ServeHTTP(w, r)
}
