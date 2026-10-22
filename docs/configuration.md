# Configuration

Every setting is a command-line flag, and every flag can also be set with an environment variable: `-share-origin` becomes `SHARTIFACTS_SHARE_ORIGIN`. If both are given, the flag on the command line wins. An empty variable counts as unset. With Docker Compose, put the variables in `.env` next to `compose.yaml` ([`.env.example`](../.env.example) lists them with comments). `shartifacts -help` prints the flags.

shartifacts checks its settings together before it listens, and it refuses to start on a combination that would expose or break the workspace. For example, a share hostname that is also a workspace hostname is refused, and so is an app wildcard that covers either of them.

## Access and modes

| Variable | Flag | Default | |
| --- | --- | --- | --- |
| `SHARTIFACTS_LISTEN` | `-listen` | `:8080` | Address to listen on. In Docker leave it alone, and use `SHARTIFACTS_BIND` instead. |
| `SHARTIFACTS_ALLOWED_HOSTS` | `-allowed-hosts` | | Extra hostnames the workspace answers to, comma-separated (`myserver.local,myserver.tailnet-name.ts.net`). LAN IPs, `localhost` and the `SHARTIFACTS_PUBLIC_ORIGIN` hostname always work. Any other `Host` is refused, which defeats DNS rebinding. |
| `SHARTIFACTS_PUBLIC_ORIGIN` | `-public-origin` | | The `https://` address of the workspace when a TLS-terminating proxy (`tailscale serve`, Caddy, nginx) fronts it. A proxy forwards plain HTTP, so shartifacts cannot see the scheme, and it does not trust forwarded headers for it. Without this setting, the page loads but its API calls are refused. MCP tools use this address in the links they hand to people. A bare hostname means `https://`. |
| `SHARTIFACTS_SHARE_ORIGIN` | `-share-origin` | | The public hostname for read-only artifact links and public app links, such as `https://share.example.com`. Setting it switches on [public sharing](sharing.md). On that hostname only share pages are served. |
| `SHARTIFACTS_SHARE_LISTEN` | `-share-listen` | `:8081` | With the share origin set, the *public listener*: it serves only share and app links whatever `Host` arrives, and it is where an edge proxy should forward. `off` turns it off. |
| `SHARTIFACTS_APP_ORIGIN` | `-app-origin` | | Needs the share origin. A wildcard origin that gives each app's public links a hostname of its own (`https://*.share.example.com`), so app pages keep browser storage and cookies. [More](sharing.md#apps-on-hostnames-of-their-own). |
| `SHARTIFACTS_AUTH_PROXY` | `-auth-proxy` | `false` | Set this only when the proxy in front of the workspace authenticates every request itself (Cloudflare Access, Authelia, oauth2-proxy). Without it, workspace requests that a proxy forwards from a public address are refused. See [security.md](security.md#behind-a-reverse-proxy). |

## Data and features

| Variable | Flag | Default | |
| --- | --- | --- | --- |
| `SHARTIFACTS_DATA` | `-data` | `./data` (`/data` in the image) | The data folder. [Back it up](install.md#back-up-and-restore). With Compose this is the folder **on the host**, mounted at `/data`. |
| `ANTHROPIC_API_KEY` | | | Lets artifacts call Claude (`window.claude.complete`, `claude.use("sample")`). Off when empty. Anyone who can reach the workspace can spend the key. |
| `SHARTIFACTS_CLAUDE_MODEL` | `-claude-model` | `claude-opus-5-5` | The model behind those calls. |
| `SHARTIFACTS_CONNECTORS` | `-connectors` | | A JSON file of MCP servers that artifacts may call through `claude.use("mcp")`. See [claude-compatibility.md](claude-compatibility.md#connectors). |
| `SHARTIFACTS_NO_CHUNK_UPLOAD` | `-no-chunk-upload` | `false` | Hides the base64 upload tools from agents, which saves their context when every agent can run `curl`. |

## Apps

| Variable | Flag | Default | |
| --- | --- | --- | --- |
| `SHARTIFACTS_RUNNER_URL` | `-runner-url` | `http://runner:8090` in Compose | Where app backends are built and run. Empty runs them inside the shartifacts process, which then needs `go` on its `PATH` to build Go backends. |
| `SHARTIFACTS_APP_PROCS` | `-app-procs` | `16` | The most app backends running at once. The least recently used one stops to make room. |
| `SHARTIFACTS_APP_IDLE` | `-app-idle` | `15m` | How long an app backend may go without requests before it stops. |
| `SHARTIFACTS_GO` | `-go` | `go` | The Go toolchain that builds Go backends. |
| `SHARTIFACTS_RUNNER` | `-runner` | `false` | Run as the runner sidecar. The runner image sets this. |
| `SHARTIFACTS_RUNNER_LISTEN` | `-runner-listen` | `:8090` | The runner's API address. Never publish it. |
| `SHARTIFACTS_APPS_DIR` | `-apps-dir` | `./data/apps` | The runner's view of the apps folder. |
| `SHARTIFACTS_RUNNER_TOKEN` | | generated | The secret shartifacts and the runner share. By default it is generated into `data/apps/.runner-token`. |

## Docker Compose only

These are read by `compose.yaml` and the files in `deploy/`, not by shartifacts itself.

| Variable | Default | |
| --- | --- | --- |
| `SHARTIFACTS_BIND` | `127.0.0.1` | The host address the workspace port is published on. Use `127.0.0.1` for this machine only (and a proxy on it), and `0.0.0.0` or a LAN IP for your network. Never forward this port from a router. |
| `SHARTIFACTS_PORT` | `8080` | The host port. |
| `SHARTIFACTS_SHARE_BIND` | `127.0.0.1` | The host address the public listener is published on, for a reverse proxy of your own. The bundled edges do not need it. It serves only share and app links, so publishing it on your network is safe. |
| `SHARTIFACTS_SHARE_PORT` | `8081` | The public listener's host port. |
| `SHARTIFACTS_DATA` | `./data` | The host folder for data. It must be writable by UID/GID 10001. |
| `SHARTIFACTS_RUNNER_MEMORY` | `2g` | The runner container's memory limit. |
| `SHARTIFACTS_GOPROXY` | `https://proxy.golang.org,direct` | Where Go modules come from when backends build. `off` builds only apps that use the standard library or vendor their modules. |
| `COMPOSE_FILE` | `compose.yaml` | Adds an edge for public sharing: `compose.yaml:deploy/caddy/compose.yaml` or `compose.yaml:deploy/cloudflare-tunnel/compose.yaml`. |
| `CLOUDFLARE_TUNNEL_TOKEN` | | The token of the Cloudflare tunnel, for the Cloudflare edge. |
| `TZ` | `UTC` | The time zone for logs. |

## Upgrading from earlier versions

`SHARTIFACTS_HOSTS` in `.env` is now `SHARTIFACTS_ALLOWED_HOSTS`, the same name the program reads. Rename the line, or those hostnames will stop being accepted. You can drop the `SHARTIFACTS_PUBLIC_ORIGIN` hostname from it, because that one is now allowed automatically.
