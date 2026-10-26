# Development

## Layout

```text
main.go                 starts the server (or the runner sidecar) from internal/config
internal/config/        flags and SHARTIFACTS_* variables, validation, the operating mode
internal/server/        HTTP: the network guard (server.go, forwarded.go), workspace API,
                        MCP tools (mcp.go, agent.go, apps_mcp.go), previews, share links
                        (share.go), apps (apps*.go), the claude.use runtime, search API
internal/store/         the data folder: artifacts, revisions, media, db, apps, shares
internal/search/        the full-text index behind library search
internal/runner/        builds and runs app backends, here or in the runner sidecar
internal/sandbox/       Landlock confinement for app backends (Linux)
web/                    the UI, embedded into the binary; web/vendor/ is generated
scripts/                build.mjs (vendored libraries) and the browser/Docker test suites
deploy/                 Compose overlays for public sharing: Caddy, Cloudflare Tunnel
skills/                 the agent setup skill
docs/                   documentation
```

The server is plain `net/http` with no framework. Each feature registers its routes on one mux, and `guard` decides who may reach anything at all. Comments explain *why*. Keep it that way: most of the code is security-relevant, and the reasoning is what a reviewer needs.

## Branches and remotes

`main` is the one long-lived branch. Work happens on a short-lived branch named for what it does (`feat/…`, `fix/…`, `docs/…`, or `t3/<slug>` when an agent made it), it is reviewed as a pull request, and the branch is deleted once it merges. `git branch -vv` should show `main` and whatever you are working on, nothing else: a leftover branch means its pull request is open or was merged without the branch being removed.

`origin` is the canonical GitHub repository and the only remote that accepts pushes. If you keep a second copy, such as a self-hosted one on your own network, make it a **read-only pull mirror** of GitHub rather than a second place to push. One writer is what keeps the two copies from disagreeing about what `main` is, and a mirror that also accepts pushes is how a repository quietly ends up with two unrelated histories and a `main` that means something different depending on where you look. Add such a copy to a checkout as a fetch-only remote (`git remote add mirror <url>`) and never push to it.

Gitea, Forgejo and GitLab all offer this as a built-in mirror setting with an interval; prefer it to a cron job, so the copy stays a platform feature rather than a script of your own.

## Build and test

```sh
go test ./...
go build ./...
go vet ./...
go test -race ./...

# Rebuild vendored libraries (web/vendor, including the per-library bundles in
# web/vendor/lib, the shadcn/ui-compatible components from scripts/ui.jsx, and
# Tailwind with scripts/tailwind-compat.js) after changing the locked npm dependencies
npm ci
npm run build

# Actual browser + separate MCP SDK client + persistence tests
npx playwright install chromium
npm run test:browser

# The apps hub in a browser: MCP deploys, tabs, a sandboxed page calling its Go backend,
# rollback, live-deploy notices, logs, secrets, public links
npm run test:apps

# The compose shape in Docker: shartifacts + the runner sidecar, and what Landlock stops
npm run test:docker-apps

# The claude.use runtime: two people at once, Claude sampling against a fake
# Messages API, and connectors against this server's own MCP endpoint
npm run test:runtime

# Container startup, Linux React compilation, and volume persistence
docker build -t shartifacts:verify .
npm run test:docker
```

`make check` runs the Go checks, and `make help` lists the other targets. CI (`.github/workflows/ci.yml`) runs `go vet`, the race-enabled tests and a Docker build on every push and pull request.

On macOS, browser tests use installed Google Chrome by default; elsewhere they use Playwright Chromium. Set `CHROME_PATH` to override the executable. The script builds an isolated binary, uses a temporary data directory, starts and stops its own server, and writes screenshots under `test-results/`. It verifies MCP upload URL discovery, direct curl uploads and MCP artifact readback, shared design tokens and rendered styles, chunked MCP media uploads, browser image uploads, real video/audio decoding and playback, video seeking and captions, media in React and unsaved drafts, byte ranges, media persistence, mobile media-library layout, all artifact types, interactive React/HTML, live drafts and recovery, revision restore, exact downloads, agent updates and conflicts, sandbox boundaries, import/search/duplicate/delete, mobile overflow, bundled React libraries with shadcn/ui and Tailwind, CDN script tags served locally, `window.storage` across reloads, the full-page view and restarts, `window.fs.readFile`, runtime error reports reaching `get_artifact`, comments over MCP, pinning and the gallery, forms, dialogs, downloads and new-tab links, and persistence after restart. Clicks into a just-loaded preview wait until Chrome routes input into the out-of-process frame (see `frameInteractive`).

Vendored browser libraries and their license notices are included in the binary. `npm run build` writes `web/vendor/THIRD_PARTY_NOTICES.txt` with the license of every npm package that ends up in a bundle (it fails if one has no license it can find), plus the fonts, Tailwind and shadcn/ui. Go and npm dependencies are locked in `go.sum` and `package-lock.json`.
