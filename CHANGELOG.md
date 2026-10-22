# Changelog

## Unreleased

Getting ready for a public release: simpler setup, clear modes, and a harder edge.

### Upgrading

- **Rename `SHARTIFACTS_HOSTS` to `SHARTIFACTS_ALLOWED_HOSTS`** in `.env`. Otherwise the hostnames it listed stop being accepted. You can drop the `SHARTIFACTS_PUBLIC_ORIGIN` hostname from the list, because it is now allowed automatically.
- **Public sharing**: point your edge proxy at the new public listener, **port 8081**, instead of 8080. Port 8080 still answers share hostnames for now, but 8081 cannot serve the workspace whatever the proxy sends. In Compose, 8081 is published on `127.0.0.1` (set `SHARTIFACTS_SHARE_BIND` for a proxy on another machine).
- If you wrote a default port into an origin (`https://share.example.com:443`), it is now dropped, so it matches what browsers send.

### Added

- **Modes** ([docs/modes.md](docs/modes.md)): private, public sharing with the bundled Caddy, and public sharing through Cloudflare Tunnel or your own proxy. The mode is logged at startup and reported by `/api/config`.
- **Agent-driven setup**: [skills/shartifacts-setup](skills/shartifacts-setup/SKILL.md), and a README that starts with a prompt to give your agent.
- **Every flag is also a `SHARTIFACTS_*` environment variable.** Compose passes settings as variables, and the image no longer needs command-line arguments.
- **Settings are checked together at startup.** shartifacts refuses to start on combinations that would expose or break the workspace: a share hostname that is also a workspace hostname, an app wildcard covering either, paths in origins.
- **Public listener** (`SHARTIFACTS_SHARE_LISTEN`, `:8081`): it serves only share and app links, whatever `Host` arrives. `off` turns it off.
- **Connect your agent**: the workspace and the apps hub share one dialog with this server's MCP address and ready-to-paste commands for Claude Code, Codex and `mcpServers` clients. Empty workspaces and hubs lead with it.
- **Bundled edges**: `deploy/caddy` uses automatic HTTPS with on-demand certificates, so app hostnames need no wildcard certificate or DNS API. `deploy/cloudflare-tunnel` needs no open ports. Both are enabled through `COMPOSE_FILE`.
- `GET /api/tls-allowed?domain=`, for proxies that get certificates on demand. It approves only the share hostname and the hostnames of existing apps.
- **Docs** split out of the README into `docs/`: modes, install, configuration, sharing, reverse proxies, security, agents, artifacts, apps, media, HTTP API, development. Also new: `SECURITY.md`, `CONTRIBUTING.md`, `AGENTS.md`, CI, and `make help`.

### Security

- **Workspace requests that a proxy forwards from a public address are refused**, judged by `X-Forwarded-For`, `Forwarded`, `CF-Connecting-IP` and similar headers. This stops a proxy that is exposed by mistake from serving the workspace to the internet. `SHARTIFACTS_AUTH_PROXY=true` opts out, for a proxy that authenticates people itself.
- Read-only app links refuse WebSocket upgrades and bare `OPTIONS` requests. Revoking a link, or deleting its app, now also ends its open WebSockets and event streams within seconds. Revoked artifact links stop their live updates too.
- A client can no longer remove the `X-Shartifacts-*` headers that backends trust by naming them in `Connection`. Backends now receive `X-Forwarded-Host` and `X-Forwarded-Proto`, as documented.
- Encoded dot segments (`%2e%2e`) in app paths are refused, rather than reaching backends as `..`.
- A share link's data calls look up the token before reading the body, and read 64 KiB at most.
- A Go backend in a custom directory (`"backend": {"go": "./backend"}`) is no longer served as static files.
- React builds are cached, so reloading a shared page does not rebuild it every time.

### Fixed

- A reconnecting page no longer drops itself from `room`s when its old stream finally closes. The old stream now ends as soon as it is replaced.
- At startup, shartifacts waits for the runner sidecar before reporting it unreachable.
- The apps hub says when it cannot load your apps, instead of showing an empty page. Warning text is readable in the light theme. Share pages no longer ask the share hostname for Claude's limits.
- The design notes in `web/MOBILE-UX.md` moved to `docs/design/mobile.md`, so the binary no longer serves them.
