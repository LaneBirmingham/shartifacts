# Modes

shartifacts is meant to be set up once, in one place, and then left alone. How you run it depends on two questions:

1. **Who opens the workspace?** Only you on this machine, everyone on your home network, or you from anywhere through Tailscale. The workspace has no login, so it is never opened from the open internet.
2. **Do you want public links?** If you do, you need a domain of your own, and something on the internet (an "edge") that brings visitors to the share hostname.

| | Private | Public sharing | Public sharing via a provider |
| --- | --- | --- | --- |
| Workspace and MCP | LAN / tailnet / this machine | LAN / tailnet / this machine | LAN / tailnet / this machine |
| Read-only artifact links, public app links | none | `share.your-domain` | `share.your-domain` |
| Domain needed | no | yes | yes |
| Open ports on the internet | none | 80 and 443 | none (Cloudflare Tunnel) or your proxy's |
| Edge | none | bundled Caddy | Cloudflare Tunnel, or your existing proxy |
| Settings | 1–2 | +2 | +2–3 |

All three are the same program. The mode depends only on whether `SHARTIFACTS_SHARE_ORIGIN` is set. At startup shartifacts logs which mode it is in, and `GET /api/config` reports it. You can move between modes at any time by changing `.env` and running `docker compose up -d`.

## Private

Nothing is reachable from the internet. Pick how far the workspace reaches with `SHARTIFACTS_BIND`:

**Only this machine** (the default; nothing to set):

```sh
docker compose up -d --build        # http://localhost:8080, MCP at http://localhost:8080/mcp
```

**Your home network**: everyone on your Wi-Fi can open it, and so can your agents on other machines:

```sh
# .env
SHARTIFACTS_BIND=0.0.0.0            # or this machine's LAN IP, e.g. 192.168.1.20
```

Open `http://192.168.1.20:8080`. Anyone on that network can read, change and delete everything, so use this on a network you trust (not a café's or a shared office's).

**You, from anywhere, over Tailscale** (recommended for remote access):

```sh
# on the host, after installing Tailscale and enabling HTTPS certificates in its admin console
tailscale serve --bg 8080
```

```sh
# .env
SHARTIFACTS_BIND=127.0.0.1
SHARTIFACTS_PUBLIC_ORIGIN=https://myserver.tailnet-name.ts.net
```

`tailscale serve` gives the workspace a real HTTPS address on your tailnet. The browser features that need a secure context (camera, microphone and clipboard in artifacts) then work, and agents on any of your devices connect to `https://myserver.tailnet-name.ts.net/mcp`. Never use `tailscale funnel` for the workspace: Funnel publishes it to the internet. You can combine this with `SHARTIFACTS_BIND=0.0.0.0` to keep LAN access as well. [reverse-proxies.md](reverse-proxies.md#tailscale-serve) has the details.

## Public sharing

You keep everything from private mode, and add a public share hostname. The bundled Caddy gets certificates for it automatically, and visitors reach it on ports 80 and 443. This suits a VPS, or a home server whose router forwards 80 and 443 to it.

```sh
# .env
COMPOSE_FILE=compose.yaml:deploy/caddy/compose.yaml
SHARTIFACTS_SHARE_ORIGIN=https://share.example.com
SHARTIFACTS_APP_ORIGIN=https://*.share.example.com     # optional: a hostname per app
```

You also need DNS records for `share.example.com` (and `*.share.example.com`) that point at the server. [sharing.md](sharing.md#caddy-bundled) walks through the steps and how to verify them.

### On a VPS

A VPS sits on the internet, so there is no home network to trust:

- Keep `SHARTIFACTS_BIND=127.0.0.1`, which is the default. Never `0.0.0.0`: rootless Docker, Podman and some firewall setups hide the client's real address, which would make every visitor look local.
- Reach the workspace over **Tailscale** (above), or with an SSH tunnel: `ssh -L 8080:localhost:8080 you@your-vps`, then open `http://localhost:8080`. Agents on your laptop use the same tunnel.
- Open only 80 and 443 to the internet, for Caddy.

## Public sharing via a provider

This is the same as public sharing, but the edge is run by a service you already use. Your setup agent can drive the provider for you if you give it an API token.

**Cloudflare Tunnel.** Your server connects out to Cloudflare, so you open no ports and your home IP stays private. This is the best fit for a home server.

```sh
# .env
COMPOSE_FILE=compose.yaml:deploy/cloudflare-tunnel/compose.yaml
CLOUDFLARE_TUNNEL_TOKEN=eyJ...
SHARTIFACTS_SHARE_ORIGIN=https://share.example.com
```

The tunnel's public hostname for `share.example.com` points at `http://shartifacts:8080`. App hostnames work on Cloudflare too, with some conditions about certificates: see [sharing.md](sharing.md#cloudflare-tunnel).

**A reverse proxy you already run.** Nginx Proxy Manager, Traefik, nginx and others work too. Leave `COMPOSE_FILE` unset, publish the port only where the proxy can reach it, and follow [reverse-proxies.md](reverse-proxies.md).

**A DNS provider's API**, for wildcard certificates by DNS-01 or for a private workspace name with a real certificate (such as `https://shartifacts.example.com` resolving to a tailnet IP). This is optional and advanced. The setup skill covers the common cases, and [reverse-proxies.md](reverse-proxies.md) shows what the proxy needs.

## Choosing quickly

- Just trying it, or only you on one computer: **Private**, default settings.
- A home server for your household: **Private** on your home network, plus Tailscale for when you are away.
- A home server, and you want to send people links: **Public sharing via Cloudflare Tunnel**.
- A VPS: **Public sharing** with Caddy, with the workspace over Tailscale.
- You already run a reverse proxy for other services: **Public sharing via your proxy**.
