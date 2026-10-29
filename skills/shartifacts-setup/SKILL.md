---
name: shartifacts-setup
description: Install, configure, verify and connect shartifacts, a self-hosted workspace and MCP server for AI-made artifacts and apps, on a laptop, home server, NAS or VPS. Covers the private mode, public sharing with the bundled Caddy, and public sharing through Cloudflare Tunnel or an existing reverse proxy, including DNS records and Tailscale. Use when someone asks to set up, install, deploy, upgrade, back up, move, troubleshoot or change the sharing mode of shartifacts.
---

# Set up shartifacts

You are setting up shartifacts for a person, end to end: questions, plan, install, verify, connect. Work in the repository's own documentation. This skill tells you the order, the decisions and the safety rules, and links to the docs for detail:

- `docs/modes.md`: the three modes and their exact settings
- `docs/install.md`: installing, upgrading, backups, platform notes, troubleshooting
- `docs/sharing.md`: public sharing, DNS, Caddy, Cloudflare Tunnel, verification
- `docs/reverse-proxies.md`: tailscale serve, nginx, Nginx Proxy Manager, Traefik
- `docs/security.md`: the security model you must keep intact
- `docs/configuration.md`: every setting
- `references/cloudflare.md`: driving Cloudflare's API (tunnel, DNS)

## What you are setting up

- The **workspace** (port 8080) is the UI plus the MCP endpoint `/mcp`. Anyone who can use it can read, change and delete everything, and can deploy code that runs on the server. It must only ever be reachable from the machine itself, the person's LAN, or their tailnet.
- **Sign-in** is optional and off until an owner password is set. Then browsers sign in once and agents send a token (`Authorization: Bearer shx_…`) that can be revoked. It keeps out people and devices on the person's own network; it never makes the workspace safe to expose to the internet.
- **Public sharing** is optional. With `SHARTIFACTS_SHARE_ORIGIN` set, a second port (8081) serves only read-only artifact links and public app links on a hostname of the person's own. An edge (the bundled Caddy, a Cloudflare tunnel, or their own proxy) brings the internet to port 8081, and only to port 8081.
- Docker Compose runs two containers, `shartifacts` and `shartifacts-runner` (app backends), plus `shartifacts-caddy` or `shartifacts-cloudflared` when an edge is enabled. All settings live in `.env` next to `compose.yaml`.

## Rules

1. **Never expose the workspace to the internet.** Do not forward port 8080, do not `tailscale funnel` it, do not route its hostname through a tunnel or a public proxy host, and never set `SHARTIFACTS_AUTH_PROXY` unless the person has an authenticating proxy (Cloudflare Access, Authelia, oauth2-proxy) and asks for it.
2. **Ask before you act on anything outside the install folder**: installing packages or Docker, changing firewall rules, router port forwarding, DNS records, Cloudflare or Tailscale settings, existing reverse proxies, or anything else already running on the machine. Show the exact change first. Read-only checks need no permission.
3. **Secrets**: when you need an API token (Cloudflare, Anthropic), ask the person to put it in `.env` themselves, or to paste it if they prefer. Never echo it back, never write it anywhere except `.env` (mode 600) or a shell variable, and never commit it. The same goes for agent tokens, which belong only in the agent's own MCP configuration. Let the person choose the owner password themselves, in the browser; do not ask them to tell it to you.
4. **Verify every step** with the checks below before moving on, and report failures honestly, with the output. A `curl` 200 proves that a socket works, not that the page does: open the UI as well when you can.
5. Prefer the defaults. Change only what the chosen mode needs.

## 1. Ask

Ask these questions in one message, with the defaults shown, and wait for the answers:

1. **Where should it run?** This machine, or another one you can reach over SSH (which host and user?). What is that machine? A Linux server or VPS, Unraid, a NAS, a Raspberry Pi, or a Mac/Windows desktop?
2. **Who should open the workspace?** (a) only that machine, (b) everyone on the home network, (c) the person from anywhere, through Tailscale. Default: (b) at home, (c) on a VPS.
3. **Public links?** No (default). Or yes: which domain and which hostname (default `share.<domain>`), who hosts its DNS, and can the internet reach that machine on ports 80/443 (a VPS, or a router they can forward ports on)? Do they already run a reverse proxy, or use Cloudflare? Do they want a hostname per app (`*.share.<domain>`; recommended if they will share apps)?
4. **A password?** Should browsers sign in and agents need a token? Recommended when guests, smart devices or other people share the network or tailnet. Default: yes for the home network and Tailscale plans, no for this machine only. They choose the password themselves.
5. **Optional extras**: an Anthropic API key so artifacts can call Claude (default: none, fully offline).
6. **Which agents** should be connected besides you (Claude Code, Codex, Cursor, VS Code, Gemini CLI, Claude Desktop)?

Do not ask what you can find out yourself (OS, architecture, Docker version, free ports, whether Tailscale is installed): check those in step 3.

## 2. Plan

Map the answers to a plan with this table, then show it to the person: the mode, what you will install, every setting in `.env`, whether sign-in will be on, and every external change (DNS records, firewall rules, port forwards, tunnel, Tailscale). Wait for a yes. Sign-in is not a setting in `.env`; it is turned on in step 7, after the checks.

| Situation | Plan |
| --- | --- |
| Only this machine | Private. `.env` can stay empty. |
| Home network | Private. `SHARTIFACTS_BIND=0.0.0.0` (or the LAN IP). |
| From anywhere | Private + Tailscale: `SHARTIFACTS_BIND=127.0.0.1` (add LAN access with `0.0.0.0` if wanted), `tailscale serve --bg 8080`, `SHARTIFACTS_PUBLIC_ORIGIN=https://<machine>.<tailnet>.ts.net`. |
| VPS | Private + Tailscale (or an SSH tunnel), and `SHARTIFACTS_BIND=127.0.0.1` always. |
| Public links, ports 80/443 reachable | Public sharing with the bundled Caddy: `COMPOSE_FILE=compose.yaml:deploy/caddy/compose.yaml`, `SHARTIFACTS_SHARE_ORIGIN`, optional `SHARTIFACTS_APP_ORIGIN`, plus DNS A/AAAA records. |
| Public links, home connection, DNS on Cloudflare (or willing to move it) | Public sharing through Cloudflare Tunnel: `COMPOSE_FILE=compose.yaml:deploy/cloudflare-tunnel/compose.yaml`, `CLOUDFLARE_TUNNEL_TOKEN`, `SHARTIFACTS_SHARE_ORIGIN`. See `references/cloudflare.md`. |
| Public links, an existing proxy (NPM, Traefik, nginx, Caddy) | Public sharing through it: forward the share hostname (and the app wildcard) to port **8081**. See `docs/reverse-proxies.md`. Set `SHARTIFACTS_SHARE_BIND` if the proxy is not on the same machine. |

For app hostnames on Cloudflare, explain the certificate constraint in `docs/sharing.md#cloudflare-tunnel` and let the person choose.

If they want apps installed on a phone as apps of their own, add `SHARTIFACTS_PRIVATE_APP_ORIGIN` (`https://*.apps.<their domain>`) to any plan with a domain and a private HTTPS proxy: a wildcard DNS record only their network resolves (or one pointing at a private or tailnet address), a wildcard certificate by DNS challenge, and the proxy forwarding those names to port **8080**, never 8081. It is never public. See `docs/apps.md#a-private-address-for-each-app`.

## 3. Check the machine

Run these on the target, read-only:

```sh
uname -sm; cat /etc/os-release 2>/dev/null | head -3
docker version --format '{{.Server.Version}}' && docker compose version
df -h . ; free -h 2>/dev/null
ss -ltn 2>/dev/null | grep -E ':(8080|8081|80|443) ' || lsof -iTCP -sTCP:LISTEN -n -P 2>/dev/null | grep -E ':(8080|8081|80|443) '
command -v tailscale && tailscale status --self 2>/dev/null | head -2
```

- **No Docker?** Ask, then install it the distribution's way, or with Docker's convenience script (`curl -fsSL https://get.docker.com | sh`) on a fresh Linux server. On a Mac or Windows desktop, ask the person to install Docker Desktop. For Unraid and NAS devices, follow `docs/install.md#platform-notes`.
- **Port taken?** Use `SHARTIFACTS_PORT` (8080) or `SHARTIFACTS_SHARE_PORT` (8081). If 80/443 are taken by an existing proxy, use the "existing proxy" plan rather than the bundled Caddy.
- Note the LAN IP (`hostname -I` or `ipconfig getifaddr en0`) for links.

## 4. Install

```sh
git clone https://github.com/LaneBirmingham/shartifacts.git ~/shartifacts   # or /opt/shartifacts
cd ~/shartifacts
cp .env.example .env && chmod 600 .env
mkdir -p data && sudo chown -R 10001:10001 data          # Linux; the containers run as UID 10001
```

Edit `.env` for the plan. Uncomment and set only the lines the plan names. On Unraid, point `SHARTIFACTS_DATA` at `/mnt/cache/appdata/shartifacts/data` (see the platform notes).

```sh
docker compose up -d --build        # the first build takes a few minutes
docker compose ps
docker compose logs shartifacts | head -5
```

The first log lines name the mode. A configuration mistake stops the container with an error that says what is wrong: fix `.env` and run `docker compose up -d` again.

## 5. Mode-specific steps

**Tailscale** (if planned): install it if missing (`curl -fsSL https://tailscale.com/install.sh | sh`, then `sudo tailscale up`, which the person completes in a browser). Ask them to enable MagicDNS and HTTPS Certificates in the admin console under DNS. Then run `sudo tailscale serve --bg 8080`, and get the name from `tailscale serve status`. Set `SHARTIFACTS_PUBLIC_ORIGIN` to it, then run `docker compose up -d`.

**Caddy edge**: create the DNS records (`docs/sharing.md#caddy-bundled`). If the DNS provider has an API and the person gives you a token, you may do it for them. For Cloudflare, use `references/cloudflare.md`, with records set to **DNS only**. Open 80/443 (TCP, plus UDP 443) in the VPS provider's firewall, or ask the person to forward them on their router. Do not open 8080 or 8081. Check that the records resolve: `dig +short share.<domain>`.

**Cloudflare Tunnel**: follow `references/cloudflare.md`. Route only the share hostname (and the app wildcard) to `http://shartifacts:8081`.

**Existing proxy**: add a host for the share hostname (and the app wildcard) that forwards to port 8081, as in `docs/reverse-proxies.md`. Change nothing else in the proxy without asking.

## 6. Verify

On the machine:

```sh
curl -fsS http://127.0.0.1:8080/healthz                   # {"status":"ok"}
curl -fsS http://127.0.0.1:8080/api/config                 # "mode" matches the plan
curl -fsS -X POST http://127.0.0.1:8080/mcp \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"setup","version":"1"}}}' | head -c 200
docker compose logs runner | head -3                       # the sandbox line: Landlock, or why not
```

From another device, for the home-network or Tailscale plans: the workspace opens at the planned address.

With public sharing, test from **outside** the network. If you are on the same network, ask the person to open the URLs on a phone with Wi-Fi off, or use a remote vantage point such as an online HTTP checker:

- `https://share.<domain>/` → 404. `https://share.<domain>/api/artifacts` → 404.
- The workspace's own address must not answer from outside.
- Make a test link and open it from outside:

  ```sh
  A=$(curl -fsS -H 'Content-Type: application/json' http://127.0.0.1:8080/api/artifacts \
       -d '{"title":"Setup check","kind":"markdown","content":"# It works"}' | sed -E 's/^\{"id":"([^"]+)".*/\1/')
  curl -fsS -H 'Content-Type: application/json' http://127.0.0.1:8080/api/artifacts/$A/shares -d '{"version":0}'
  ```

  Open the returned `url` from outside. It should show "It works". Then delete the test artifact (`curl -X DELETE "http://127.0.0.1:8080/api/artifacts/$A?base_version=1"`), which also ends its link.
- With app hostnames: the first visit to an app's hostname takes a few seconds while Caddy gets its certificate.

If sign-in is already on (an upgrade, or data from an earlier install), the API checks above answer `401`: make a token as in step 7 and add `-H "Authorization: Bearer $TOKEN"` to them.

## 7. Sign-in and agents

**Sign-in, if planned.** Ask the person to open the workspace, choose **Set a password** (the lock in the top bar, or **Sign-in** in the menu at the foot of the sidebar), and save the password in their password manager. Then check:

```sh
docker compose exec shartifacts shartifacts auth status          # "Sign-in is on"
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/api/config   # 401
```

If they would rather you did it, `docker compose exec shartifacts shartifacts auth new-password` makes one up and prints it once; tell them to store it, and do not repeat it afterwards. A lost password is reset the same way, or with `auth set-password`.

**Connect agents.** Connect yourself first, then the others the person named (`docs/agents.md` has every client). Use the workspace address that works from where each agent runs: `http://<LAN-IP>:8080/mcp`, `https://<machine>.<tailnet>.ts.net/mcp`, or `http://localhost:8080/mcp` on the same machine or through an SSH tunnel.

Without sign-in:

```sh
claude mcp add --transport http --scope user shartifacts <URL>     # Claude Code
codex mcp add shartifacts --url <URL>                              # Codex
```

With sign-in on, give each agent its own token, named for it, so the person can revoke one alone. Keep the token in a shell variable and pass it straight to the client:

```sh
TOKEN=$(docker compose exec -T shartifacts shartifacts auth token "Claude Code on <machine>")
claude mcp add --transport http --scope user shartifacts <URL> --header "Authorization: Bearer $TOKEN"
```

For Codex, the token goes in an environment variable that Codex reads (`--bearer-token-env-var SHARTIFACTS_TOKEN`), which the person must also export in their shell profile; the person can instead use **Connect an agent** in the workspace, which makes a token and shows the commands with it filled in.

Then prove that it works end to end: create a small artifact through the MCP tools (for example a one-screen "Hello from <agent>" HTML page), give the person its link, and delete it afterwards if they don't want it.

## 8. Hand over

Finish with a short summary for the person:

- The workspace address(es) and the MCP URL; the share hostname if any
- The mode, and what is reachable from where
- Whether sign-in is on, the names of the tokens you made, and that **Sign-in** in the workspace revokes them
- Where the install and data folders are, how to back up (`docs/install.md#back-up-and-restore`) and upgrade (`git pull && docker compose up -d --build`)
- Anything left for them to do (a router port forward, a Tailscale admin toggle), and anything that failed

## Later: changes and problems

- **Change mode**: edit `.env` (add or remove `SHARTIFACTS_SHARE_ORIGIN` and `COMPOSE_FILE`), then `docker compose up -d`. Turning sharing off ends public access at once. Links come back if it is turned on again with the same hostname.
- **Move to another machine**: stop the stack, copy the data folder and `.env`, install there, then start.
- **Upgrade**: read `CHANGELOG.md` for `.env` changes, then `git pull && docker compose up -d --build`.
- **Sign-in**: turn it on or off, change the password or revoke tokens under **Sign-in** in the workspace. A lost password: `docker compose exec shartifacts shartifacts auth set-password`; it lists the agent tokens that still work, and if someone else may have known the old password, `… auth revoke all` ends them (then reconnect the agents with new ones). An agent that suddenly gets `401` needs a token (or a new one, if its token was revoked).
- **Troubleshooting**: `docs/install.md#troubleshooting`. Look at `docker compose logs shartifacts` first: refusals log why.
