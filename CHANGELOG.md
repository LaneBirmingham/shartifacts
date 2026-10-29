# Changelog

## Unreleased

### Fixed

- **The workspace scrolls on the GPU on Windows and Linux** ([docs/apps.md](docs/apps.md)). The apps home, the library, the details panel, the source pane, app drawers and the tab strip each paint the colour behind them. Chrome draws text for LCD screens there, and it re-rastered a scroller it could see through on every frame: the apps home cost about 15 raster tasks a frame at 1×, now none. On a 2560×1440 screen at 1× a frame of it takes 2.6 ms instead of 4.2 (a 120 Hz screen allows 8.3), and a third as many frames overrun. Nothing looks different. The app build guide now asks the same of apps' own scrolling panels, and to animate with CSS rather than script: Chrome for Android runs a page's script and `requestAnimationFrame` at 60 Hz on a 120 Hz screen once the finger lifts, and Safari halves `requestAnimationFrame` in a hub tab until the app is tapped.

### Changed

- **Artifacts and Apps switch in the same place** ([docs/artifacts.md](docs/artifacts.md#editing)). An **Artifacts · Apps** switch sits under the logo on both pages and lights the side you are on, so going to Apps and coming back is the same move. It replaces the Apps entry in the sidebar menu and the Artifacts button in the apps bar; the apps bar's home tab is now the house icon alone.
- **An open artifact's controls are sorted by how often you need them** ([docs/artifacts.md](docs/artifacts.md#editing), [docs/design/mobile.md](docs/design/mobile.md)). The toolbar holds the title, version, the Preview / Source / Split / Changes switch, Comment, Comments, a **Share** menu (copy link, open in a new tab, download, public read-only link) and **ⓘ**. **ⓘ** (or **I**) opens a details panel, as in Google Drive and Figma, with **About** (description, tags, dates, related artifacts, and the actions, with Archive and Delete last and apart), **Versions** (every version with its note) and **Comments**. **Edit source** sits on the Source pane (or press **E**), Save and Discard sit with the edit fields, and an earlier version shows a notice with **Restore this version** and **Back to latest**. The **Details** drop-down, the ••• menu and the footer under the panes are gone. Your name (**Who are you?**) moved to the foot of the sidebar.
- **New artifact moved into the ⋯ menu** at the foot of the sidebar ([docs/artifacts.md](docs/artifacts.md#editing)), since agents make most artifacts; the library starts right under the search. **N** still opens it, as does **Paste or import source** on the home page.

### Added

- **Focus mode for apps** ([docs/apps.md](docs/apps.md)). **⤢ Focus** fills the window with an app, growing its own frame over the hub with a transform-only animation, so nothing reloads. A pill at the top edge (which tucks into a handle), Back, and **Esc** (twice from inside the app) bring the hub back, and `/apps/{id}?focus` opens an app focused. The installed app hides **Open ↗**, which used to strand you on the bare app page with no way back. App pages now tell the hub when Escape is pressed, by key name only.
- **A port registry for the network** ([docs/ports.md](docs/ports.md)). Agents that deploy things around the network — a Docker stack on the server, a dev server — claim the ports they publish, with the new `ports` tool or `/api/ports`, and see what every other agent and person claimed. A claim without a port takes the next free one atomically, so two agents asking at once never get the same one, and a claim on a taken port is refused with its holder and the next free port. **Ports in use** on the apps home page lists them by machine and releases them. It is a tool of its own because no existing one fits: the app tools manage shartifacts apps, whose backends already get a free `$PORT` and never need one from here.

## 2026.10.28

### Changed

- **CI runs on the server and deploys it** ([docs/development.md](docs/development.md#continuous-integration-and-deploying)). Builds and tests run in Gitea Actions, on pull requests and on `main`; a green `main` is copied to the compose directory and rebuilt by `scripts/deploy-tower.sh`, which also records the running commit and keeps the previous images for a rollback that needs no rebuild. The public GitHub repository is now a read-only mirror of the Gitea one and carries no workflows at all.
- **Versions are calendar versions** — `YYYY.MM.DD`, and `.2` for a second release the same day — in the git tag, in `CHANGELOG.md` and in the version an agent sees in the MCP handshake. `v0.1.0` and `v0.2.0` keep their names.

## 0.2.0 — 2026-10-22

Cheaper context for every connected agent: the same tools, fields and behaviour, at about 4.2k tokens instead of 5.5k.

### Changed

- **Agents spend less context on shartifacts** ([docs/agents.md](docs/agents.md)). The instructions an agent receives when it connects are now a short summary: the artifact kinds, `base_version` before updating, what sandboxed previews refuse, the design system, archiving, links and the upload URL and `curl` command as before. The full guide they used to carry is the MCP resource `shartifacts://guide` (`resources/list`, `resources/read`), unchanged, for agents to read when they need it. Tool fields whose meaning is plain from their name and type, such as `title`, `kind`, `limit` and `note`, no longer carry a description; the ones with rules, defaults or valid values keep theirs. The tools, their fields and their behaviour are unchanged.
- **The test suite is about twice as quick** ([docs/development.md](docs/development.md#continuous-integration-and-deploying)). Each test that deploys an app builds a Go backend in its own temporary apps directory, and every one of those builds compiled the standard library again — eight seconds a backend. They now share one Go build cache for the whole test binary, which takes the race-enabled suite from about forty seconds to twenty-five on a laptop, and CI's `go test -race` step from 151 seconds to 38.

## 0.1.0 — 2026-10-22

The first public release: simpler setup, clear modes, and a harder edge.

### Upgrading

- **Rename `SHARTIFACTS_HOSTS` to `SHARTIFACTS_ALLOWED_HOSTS`** in `.env`. Otherwise the hostnames it listed stop being accepted. You can drop the `SHARTIFACTS_PUBLIC_ORIGIN` hostname from the list, because it is now allowed automatically.
- **Public sharing**: point your edge proxy at the new public listener, **port 8081**, instead of 8080. Port 8080 still answers share hostnames for now, but 8081 cannot serve the workspace whatever the proxy sends. In Compose, 8081 is published on `127.0.0.1` (set `SHARTIFACTS_SHARE_BIND` for a proxy on another machine).
- **Sign-in** is new and off until you set a password, so nothing changes on upgrade. Once you set one, agents you connected before need a token.
- If you wrote a default port into an origin (`https://share.example.com:443`), it is now dropped, so it matches what browsers send.

### Added

- **Archive** ([docs/artifacts.md](docs/artifacts.md#editing)): archive artifacts, one or many at once with **Select**, to keep the library short. Archived artifacts keep everything and still open by their link, but leave the library, search and related lists until restored; the **Archive** view lists, searches, restores and permanently deletes them. Agents tidy up with `update_artifact`'s new `archived` (and `ids` for many at once) and empty the archive with `delete_artifact`'s new `ids`, without a confirmation round for archived artifacts; `list_artifacts` and `search_artifacts` leave the archive out unless `archived` asks, and search sorts by `oldest`. Over HTTP: `POST /api/artifacts/bulk` and `?archived=`.
- **Optional sign-in** ([docs/security.md](docs/security.md#sign-in)): set an owner password under **Sign-in** in the workspace, or with `shartifacts auth set-password`, and browsers sign in once (30-day sessions) while agents send a revocable token per agent (`Authorization: Bearer shx_…`). **Connect an agent** makes a token and fills it into the commands. Share links and app links stay public. `shartifacts auth` also makes and revokes tokens and resets a lost password from the server's shell. Passwords are stored as PBKDF2 hashes and tokens as digests; failed logins are throttled; app sessions and upload URLs end with the sign-in they were made for; tokens never reach app backends.
- **Modes** ([docs/modes.md](docs/modes.md)): private, public sharing with the bundled Caddy, and public sharing through Cloudflare Tunnel or your own proxy. The mode is logged at startup and reported by `/api/config`.
- **Agent-driven setup**: [skills/shartifacts-setup](skills/shartifacts-setup/SKILL.md), and a README that starts with a prompt to give your agent.
- **Every flag is also a `SHARTIFACTS_*` environment variable.** Compose passes settings as variables, and the image no longer needs command-line arguments.
- **Settings are checked together at startup.** shartifacts refuses to start on combinations that would expose or break the workspace: a share hostname that is also a workspace hostname, an app wildcard covering either, paths in origins.
- **Public listener** (`SHARTIFACTS_SHARE_LISTEN`, `:8081`): it serves only share and app links, whatever `Host` arrives. `off` turns it off.
- **Connect your agent**: the workspace and the apps hub share one dialog with this server's MCP address and ready-to-paste commands for Claude Code, Codex and `mcpServers` clients. Empty workspaces and hubs lead with it.
- **Bundled edges**: `deploy/caddy` uses automatic HTTPS with on-demand certificates, so app hostnames need no wildcard certificate or DNS API. `deploy/cloudflare-tunnel` needs no open ports. Both are enabled through `COMPOSE_FILE`.
- `GET /api/tls-allowed?domain=`, for proxies that get certificates on demand. It approves only the share hostname and the hostnames of existing apps.
- **Themes**: a new default palette, Terracotta (greige paper, a fired-brick primary whose white button text passes WCAG AA), and under **Theme** in the sidebar menu two more for the workspace, Harbour and Sage, saved per browser. The design system agents get uses Terracotta. Its tokens `--accent-teal`/`--accent-amber` are now `--accent-slate`/`--accent-olive`, and `.badge-coral` is `.badge-primary` (the old class still works).
- **Third-party notices**: `web/vendor/THIRD_PARTY_NOTICES.txt`, generated by the build, carries the license of everything bundled.
- **App versions cost less disk.** Every deploy, not only `merge`, hard-links the files it didn't change to the previous version's copies. A Go backend whose code didn't change reuses the previous build instead of running `go build`, and an unchanged prebuilt binary is shared.
- **Rollback window** (`SHARTIFACTS_APP_KEEP_VERSIONS`, default 20): the newest versions that built keep their files, so a run of failed builds no longer pushes good versions out. The Versions panel and `get_app` show how far rollback reaches.
- **Storage per app**: the app's Settings panel shows the disk its versions and backend data take (`GET /api/apps/{id}/usage`). `SHARTIFACTS_APP_DATA_QUOTA` caps each app's backend data.
- **Media clean-up**: the Media library can find files no artifact or app names, and delete files (`DELETE /api/media/{id}`). It names what still uses a file before deleting it.
- **Agent lists page.** `list_artifacts` and `list_media` return 50 at a time, with `next_cursor`. `list_media` also filters by name and type. `get_app` returns the newest 10 versions plus the live and linked ones, or every version with `all_versions`.
- **Docs** split out of the README into `docs/`: modes, install, configuration, sharing, reverse proxies, security, agents, artifacts, apps, media, HTTP API, development. Also new: `SECURITY.md`, `CONTRIBUTING.md`, `AGENTS.md`, CI, and `make help`.

### Security

- **Workspace requests that a proxy forwards from a public address are refused**, judged by `X-Forwarded-For`, `Forwarded`, `CF-Connecting-IP` and similar headers. This stops a proxy that is exposed by mistake from serving the workspace to the internet. `SHARTIFACTS_AUTH_PROXY=true` opts out, for a proxy that authenticates people itself.
- Read-only app links refuse WebSocket upgrades and bare `OPTIONS` requests. Revoking a link, or deleting its app, now also ends its open WebSockets and event streams within seconds. Revoked artifact links stop their live updates too.
- A client can no longer remove the `X-Shartifacts-*` headers that backends trust by naming them in `Connection`. Backends now receive `X-Forwarded-Host` and `X-Forwarded-Proto`, as documented.
- Encoded dot segments (`%2e%2e`) are refused in every path, not only app paths, rather than reaching backends or file lookups as `..`. The workspace's pages are served only by their routes (`/hub.html` is a 404), so they always carry the CSP that stops other sites from framing them.
- A share link's data calls look up the token before reading the body, and read 64 KiB at most.
- A Go backend in a custom directory (`"backend": {"go": "./backend"}`) is no longer served as static files.
- React builds are cached, so reloading a shared page does not rebuild it every time.

### Fixed

- Saving one artifact no longer holds up every other read and write while its file reaches the disk, and deploying no longer blocks the workspace while it deletes pruned versions. Artifact files are written compactly.
- The runner forgets the output of backends that ended a day ago, keeping the latest 32, rather than every version it ever started.
- A reconnecting page no longer drops itself from `room`s when its old stream finally closes. The old stream now ends as soon as it is replaced.
- At startup, shartifacts waits for the runner sidecar before reporting it unreachable.
- The apps hub says when it cannot load your apps, instead of showing an empty page. Warning text is readable in the light theme. Share pages no longer ask the share hostname for Claude's limits.
- The design notes in `web/MOBILE-UX.md` moved to `docs/design/mobile.md`, so the binary no longer serves them.
