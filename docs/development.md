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

`main` is the one long-lived branch. Work happens on a short-lived branch named for what it does (`feat/…`, `fix/…`, `docs/…`, or `t3/<slug>` when an agent made it), it is reviewed as a pull request on the Gitea server, and the branch is deleted once it merges. `git branch -vv` should show `main` and whatever you are working on, nothing else: a leftover branch means its pull request is open or was merged without the branch being removed. `main` takes no direct pushes — the branch protection there requires a pull request and a green `verify` job.

**Gitea is the canonical repository and the only remote that accepts pushes.** On a checkout it is `origin` (`http://192.168.0.222:3130/lanebirm/shartifacts.git`). `github` is the public copy and is a **push mirror configured on the Gitea server**: Gitea force-pushes to `github.com/LaneBirmingham/shartifacts` on every commit and on a ten-minute interval, so the public repository is a copy of this one and never a place to write. Its push URL is set to `DISABLED` so a stray `git push github` fails instead of quietly diverging — one writer is what keeps the two copies from disagreeing about what `main` is.

Gitea, Forgejo and GitLab all offer the mirror as a built-in repository setting; prefer it to a cron job, so the copy stays a platform feature rather than a script of your own.

## Continuous integration and deploying

CI is Gitea Actions, in `.gitea/workflows/ci.yml` — deliberately not `.github/`, so mirroring the repository to GitHub cannot start a build there (GitHub does not read `.gitea/`, and Actions are switched off on that repository as well).

- A branch or pull request runs `verify`: `gofmt -l .`, `go vet`, `go test -race`, a build, the three compose configurations, and `docker compose build`.
- A push to `main` runs `verify` and then `deploy`, which needs it to pass. Deploy is `scripts/deploy-tower.sh`: it copies the checkout to the server's compose directory, refuses to run if `.env` would change, rebuilds both images, waits for both healthchecks, tags the images with the commit and writes the commit to `.deployed`.

```sh
tower ssh 'cat /mnt/user/projects/shartifacts/.deployed'      # what is live
SHS=~/Dev/shartifacts; SRC=$SHS sh $SHS/scripts/deploy-tower.sh   # ship by hand, same script
```

Rolling back needs no rebuild — the previous images are kept:

```sh
tower ssh 'cd /mnt/user/projects/shartifacts && docker tag shartifacts:rollback shartifacts:local \
  && docker tag shartifacts-runner:rollback shartifacts-runner:local && docker compose up -d --no-build'
```

Jobs run in `deploy/ci/Dockerfile` (`shartifacts-ci:local`, built on the server) on a runner registered to this repository alone, with the docker socket and the compose directory mounted. The browser and apps suites are not in CI — they need Chrome, and they stay a local gate before a push.

## Releasing

Versions are calendar versions: `YYYY.MM.DD`, which is also the git tag (`2026.10.28`), and `.2`, `.3` after it when more than one release ships in a day. `v0.1.0` and `v0.2.0` are history and stay as they are.

A release is one commit on the work branch, before it merges, so the tagged commit is the one `main` ends up with:

1. `CHANGELOG.md`: turn `## Unreleased` into `## YYYY.MM.DD`, newest first, and leave a fresh `## Unreleased` above it.
2. `internal/server/mcp.go`: `appVersion` to the same date — it is what every connected agent sees in the MCP handshake.
3. Commit as `docs(release): YYYY.MM.DD`, merge the pull request, then tag that commit and push the tag: `git tag -a 2026.10.28 -m "2026.10.28" && git push origin 2026.10.28`. The tag reaches GitHub with the mirror.

The deploy that runs on the merge is the release.

The **GitHub release** is the one thing a mirror does not carry: it copies refs, not release notes. Make it once the tag has arrived (within ten minutes):

```sh
git ls-remote github refs/tags/2026.10.28          # an empty answer means the mirror has not caught up
sed -n '/^## 2026.10.28/,/^## /p' CHANGELOG.md | sed '1d;$d' > /tmp/notes.md
gh release create 2026.10.28 --repo LaneBirmingham/shartifacts \
  --title "2026.10.28 — <what it is>" --notes-file /tmp/notes.md
```

It is a publishing act, not a build, so it stays manual rather than a CI job with a GitHub token in the Gitea secrets. `v0.1.0` and `v0.2.0` keep their releases: deleting one leaves the tag behind and loses the notes, and the dates on the release page already show which numbering came first.

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
