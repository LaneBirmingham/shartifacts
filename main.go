package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/LaneBirmingham/shartifacts/internal/config"
	"github.com/LaneBirmingham/shartifacts/internal/runner"
	"github.com/LaneBirmingham/shartifacts/internal/sandbox"
	"github.com/LaneBirmingham/shartifacts/internal/server"
	"github.com/LaneBirmingham/shartifacts/internal/store"
)

//go:embed web
var assets embed.FS

func main() {
	// Re-executed to confine an app backend: apply the sandbox, then become the app.
	if len(os.Args) > 1 && os.Args[1] == sandbox.Arg {
		sandbox.Trampoline()
	}
	if len(os.Args) > 1 && os.Args[1] == sandbox.OKArg {
		return
	}
	cfg, err := config.Parse(os.Args[1:], os.Getenv, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if errors.Is(err, config.ErrUsage) {
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
	if cfg.Runner {
		runRunner(cfg)
		return
	}
	st, e := store.Open(cfg.Data)
	if e != nil {
		log.Fatal(e)
	}
	web, _ := fs.Sub(assets, "web")
	app := &server.Server{Store: st, Web: web, AllowedHosts: cfg.AllowedHosts, NoChunkUpload: cfg.NoChunkUpload, PublicOrigin: cfg.PublicOrigin, ShareOrigin: cfg.ShareOrigin, AppOrigin: cfg.AppOrigin, AuthProxy: cfg.AuthProxy}
	for _, line := range cfg.Summary() {
		log.Print(line)
	}
	if cfg.Connectors != "" {
		if app.Connectors, e = server.LoadConnectors(cfg.Connectors); e != nil {
			log.Fatal(e)
		}
		log.Printf("%d connectors available to artifacts", len(app.Connectors))
	}
	var local *runner.Local
	if cfg.RunnerURL != "" {
		u, err := url.Parse(cfg.RunnerURL)
		if err != nil || u.Host == "" {
			log.Fatalf("-runner-url %q is not a URL like http://runner:8090", cfg.RunnerURL)
		}
		token, err := runner.EnsureToken(st.AppsDir())
		if err != nil {
			log.Fatal(err)
		}
		app.Runtime = &runner.Remote{URL: u, Token: token}
	} else {
		local = &runner.Local{Dir: st.AppsDir(), GoBinary: cfg.GoBinary, MaxProcs: cfg.AppProcs, Idle: cfg.AppIdle}
		app.Runtime = local
	}
	go func() {
		// Compose starts the runner sidecar only once this server is healthy, so give it a
		// minute before calling it unreachable.
		info := app.Runtime.Info(context.Background())
		for wait := 0; info.Error != "" && cfg.RunnerURL != "" && wait < 60; wait += 2 {
			time.Sleep(2 * time.Second)
			info = app.Runtime.Info(context.Background())
		}
		switch {
		case info.Error != "":
			log.Printf("apps: %s", info.Error)
		case info.Go == "":
			log.Printf("apps: backends run %s; no Go toolchain, so only prebuilt backends deploy; sandbox: %s", map[bool]string{true: "in the runner", false: "here"}[info.Remote], info.Sandbox.Detail)
		default:
			log.Printf("apps: backends run %s with %s; sandbox: %s", map[bool]string{true: "in the runner", false: "here"}[info.Remote], info.Go, info.Sandbox.Detail)
		}
	}()
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		app.Claude = server.NewClaude(key, cfg.ClaudeModel)
		log.Printf("window.claude.complete enabled with %s", cfg.ClaudeModel)
	}
	newServer := func(addr string, h http.Handler) *http.Server {
		return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 5 * time.Minute, WriteTimeout: 5 * time.Minute, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
	}
	srv := newServer(cfg.Listen, app.Handler())
	var public *http.Server
	if cfg.ShareOrigin != "" && cfg.ShareListen != "" {
		public = newServer(cfg.ShareListen, app.PublicHandler())
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if public != nil {
			public.Shutdown(c)
		}
		srv.Shutdown(c)
		if local != nil {
			local.Close()
		}
	}()
	go app.WarmSearch()
	if public != nil {
		go func() {
			log.Printf("public share and app links on %s (only these, for your edge proxy)", cfg.ShareListen)
			if e := public.ListenAndServe(); e != nil && e != http.ErrServerClosed {
				log.Fatal(e)
			}
		}()
	}
	log.Printf("shartifacts listening on %s; MCP at /mcp; data at %s", cfg.Listen, cfg.Data)
	if e = srv.ListenAndServe(); e != nil && e != http.ErrServerClosed {
		log.Fatal(e)
	}
}

// runRunner serves the runner API: the sidecar that builds and runs app backends with only
// the apps directory mounted.
func runRunner(cfg *config.Config) {
	addr, dir := cfg.RunnerListen, cfg.AppsDir
	if dir == "" {
		dir = "./data/apps"
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		log.Fatal(err)
	}
	l := &runner.Local{Dir: dir, GoBinary: cfg.GoBinary, MaxProcs: cfg.AppProcs, Idle: cfg.AppIdle, Remote: true}
	info := l.Info(context.Background())
	goVersion := info.Go
	if goVersion == "" {
		goVersion = "no Go toolchain (only prebuilt backends deploy)"
	}
	log.Printf("app runner on %s for %s; %s; sandbox: %s", addr, dir, goVersion, info.Sandbox.Detail)
	srv := &http.Server{Addr: addr, Handler: runner.Handler(l), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(c)
		l.Close()
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
