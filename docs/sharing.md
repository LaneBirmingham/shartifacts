# Public sharing

Public sharing lets people outside your network open what you choose to share:

- **Read-only artifact links**: `https://share.example.com/s/<token>` shows one artifact, live or pinned to a version. Visitors can read but never change anything.
- **Public app links**: `https://share.example.com/p/<token>/` runs a whole app, frontend and backend, for anyone with the link. Links can be read-only.

Both are served on a **share hostname** of your own, which is a different name from the workspace's. Each link is a 128-bit random token: revoking it ends it at once (open streams close within seconds), and deleting the artifact or app ends all of its links.

With sharing on, shartifacts opens a second port, **8081**, the *public listener*. It serves only share pages, app links and the files they load, whatever `Host` a proxy sends. The workspace, `/api` and `/mcp` answer `404` there. **Point your edge at port 8081, never at 8080.** Port 8080 also answers the share hostname by its `Host`, for older setups, but a proxy that rewrites `Host` to its upstream address there (nginx's default) would reach the workspace.

Sharing is off until you set `SHARTIFACTS_SHARE_ORIGIN`. [modes.md](modes.md) helps you choose a setup.

## What you need

1. **A domain you control**, for example `example.com`, and a name under it for sharing, such as `share.example.com`.
2. **An edge**: something on the internet that terminates HTTPS for that name and forwards requests to shartifacts, passing the visitor's `Host` header through. Pick one:
   - [Caddy](#caddy-bundled) (bundled): the internet reaches your server on ports 80 and 443, as on a VPS or a home server with port forwarding. Certificates are automatic.
   - [Cloudflare Tunnel](#cloudflare-tunnel): no open ports or public IP needed, which suits a home connection. Your domain's DNS must be on Cloudflare.
   - [Your own reverse proxy](reverse-proxies.md): nginx, Nginx Proxy Manager, Traefik or another proxy you already run.
3. Optionally, **a hostname per app** (`SHARTIFACTS_APP_ORIGIN`), so app pages keep browser storage. See [below](#apps-on-hostnames-of-their-own).

Whatever the edge, route **only** the share hostname (and the app hostnames), and route them to port **8081**. Never route the workspace's own hostname, or port 8080, from the internet: it is built for your networks only, and its sign-in is optional. If a proxy does forward a workspace request from a public address, shartifacts refuses it (see [security.md](security.md#behind-a-reverse-proxy)), but do not rely on that alone.

## Caddy (bundled)

The bundled Caddy edge has no hostnames written into its configuration. Caddy gets a certificate for a name the first time someone opens it, and only after shartifacts confirms that it serves that name: the share hostname, or the hostname of an app that exists. This is [on-demand TLS](https://caddyserver.com/docs/automatic-https#on-demand-tls), and it checks with `GET /api/tls-allowed?domain=…`. The workspace's names never get a public certificate or route, and app hostnames need no wildcard certificate and no DNS provider API.

1. **DNS.** Point the share hostname at your server's public IP, and point a wildcard at it too if you want app hostnames:

   ```text
   share.example.com     A     203.0.113.10
   *.share.example.com   A     203.0.113.10     (only for SHARTIFACTS_APP_ORIGIN)
   ```

   Add matching `AAAA` records if the server has IPv6. If you use Cloudflare DNS, make these records **DNS only** (grey cloud), because Caddy terminates TLS itself.
2. **Ports.** Ports 80 and 443 (TCP, and UDP 443 for HTTP/3) must reach this machine. On a VPS, allow them in the provider's firewall. At home, forward them from your router to this machine. Do **not** forward 8080.
3. **Settings** in `.env`:

   ```sh
   COMPOSE_FILE=compose.yaml:deploy/caddy/compose.yaml
   SHARTIFACTS_SHARE_ORIGIN=https://share.example.com
   SHARTIFACTS_APP_ORIGIN=https://*.share.example.com    # optional
   ```

   Keep `SHARTIFACTS_BIND=127.0.0.1` on a VPS, and reach the workspace through Tailscale or an SSH tunnel ([modes.md](modes.md#on-a-vps)).
4. `docker compose up -d`, then [verify](#verify).

The Caddyfile is [`deploy/caddy/Caddyfile`](../deploy/caddy/Caddyfile). It forwards visitors to the public listener (`shartifacts:8081`), asks the private port whether a name may have a certificate, and limits public paths to `/s/`, `/p/`, `/vendor/` and the few scripts and styles share pages load.

## Cloudflare Tunnel

A tunnel connects outward from your server to Cloudflare, so nothing listens on the internet and your home IP stays hidden. Cloudflare terminates TLS.

1. **Create a tunnel.** In the Cloudflare dashboard go to *Zero Trust → Networks → Tunnels → Create a tunnel → Cloudflared*. Name it `shartifacts` and copy its token. Your agent can do this through the API instead (see the setup skill's [Cloudflare reference](../skills/shartifacts-setup/references/cloudflare.md)).
2. **Add a public hostname** to the tunnel: `share.example.com`, service `http://shartifacts:8081`. Cloudflare creates the DNS record. Add no other hostnames, and certainly not the workspace's.
3. **Settings** in `.env`:

   ```sh
   COMPOSE_FILE=compose.yaml:deploy/cloudflare-tunnel/compose.yaml
   CLOUDFLARE_TUNNEL_TOKEN=eyJ...
   SHARTIFACTS_SHARE_ORIGIN=https://share.example.com
   ```

4. `docker compose up -d`, then [verify](#verify).

**App hostnames on Cloudflare.** Cloudflare's free certificate covers `example.com` and `*.example.com`, one level deep only, so it does not cover `*.share.example.com`. You have three options:

- Buy Advanced Certificate Manager for `*.share.example.com`.
- Give shartifacts a domain of its own, with the share hostname on the apex: `SHARTIFACTS_SHARE_ORIGIN=https://example.net` and `SHARTIFACTS_APP_ORIGIN=https://*.example.net`.
- Leave `SHARTIFACTS_APP_ORIGIN` unset. App links then share the share hostname, and app pages keep storage only for the page.

For app hostnames, add a public hostname `*.example.net` (or `*.share.example.com`) with the same service, `http://shartifacts:8081`.

If **Bot Fight Mode** is on for the zone, Cloudflare may challenge visitors and link previews. Cloudflare's free plan cannot exempt one hostname from it.

## Verify

Run these from outside your network, for example on a phone with Wi-Fi off. Replace the names with your own.

```sh
curl -s -o /dev/null -w '%{http_code}\n' https://share.example.com/              # 404
curl -s -o /dev/null -w '%{http_code}\n' https://share.example.com/api/artifacts # 404
curl -s -o /dev/null -w '%{http_code}\n' -X POST https://share.example.com/mcp   # 404
curl -s -o /dev/null -w '%{http_code}\n' https://share.example.com/runtime.js    # 200
```

Then share something and open the link from outside:

- **An artifact**: open it, choose **More → Share read-only link…**, then **Link that follows new versions**. The link should open with its images. After you revoke it, it should return `404`.
- **An app**: open its tab, then **Share**. With `SHARTIFACTS_APP_ORIGIN` set, the link opens on the app's own hostname.

The workspace's hostname must **not** answer from outside. If it does, fix the edge before anything else.

## Share links in detail

**More → Share read-only link…** makes a link that follows new versions or stays on the version shown, and revokes links (the API needs `version` said outright: a number, or `0` to follow new versions). A link pinned to a version pins the page, not its data: shared `db` documents, shared storage and assets are the artifact's current ones, live, as the page would show them.

Every link in the list, for artifacts and apps alike, has a **QR code** button. It shows the link as a large QR code over the page, so people in the room can scan it with a phone camera and open the link directly. The code is drawn in the browser; the link goes nowhere else. Click or press Esc to close it.

What a visitor gets: the artifact and its files, the read-only runtime (shared `db` documents and shared `window.storage` to read, live as they change; no writes, no Claude, rooms, uploads or comments; private `data/users/<id>/` documents, anything a rule keeps above `view`, and personal `window.storage` stay hidden, as for a new person in Claude), and downloads.

What a token grants, and nothing more: every read of an artifact on the share hostname is under `/s/<token>/` and resolved through the token, so a visitor reads nothing of any artifact without one, and with token A nothing of artifact B. Token A reads A's shared version and files, the media that version references (`/s/<token>/media/<id>`), A's own assets (`/s/<token>/_blob/<id>`), A's readable data, and live notice of changes to exactly that. The workspace's `/media/` and `/_blob/` paths are `404` there; shared pages are served with their media and asset URLs (in source and in shared data) pointed at the token's paths. Media only a `db` document mentions, not the source, is not served to visitors.

How it stays safe: on the public listener, and on the share hostname, this server answers only `/s/…`, `/p/…`, the bundled libraries, `/runtime.js` and the page's own scripts and styles, with `POST` only for the share link's read-only data calls (64 KiB at most, read only after the token is found) and app backends. Everything else there is `404`: the workspace, `/api`, `/mcp`, `/media/`, `/_blob/`. That holds whatever the proxy forwards, but restrict paths at the proxy too.

What the edge must do: terminate TLS, forward to port 8081, pass the visitor's `Host` header through (needed for app hostnames; on the share hostname any `Host` is answered as the share hostname), turn buffering off for `/s/<token>/api/live` and `/p/<token>/api/…` streams (server-sent events), and allow WebSocket upgrades for apps. In Compose, port 8081 is published on `127.0.0.1` for a proxy on the same machine; set `SHARTIFACTS_SHARE_BIND` for a proxy elsewhere on your network.

## Apps on hostnames of their own

On the share hostname every app link shares one origin, so app pages there stay in the opaque sandbox: no cookies, and storage lasting only for the page. Give each app a hostname of its own and its pages keep real `localStorage`, `sessionStorage`, IndexedDB and cookies, like any site, apart from every other app's:

```sh
SHARTIFACTS_SHARE_ORIGIN=https://share.example.com
SHARTIFACTS_APP_ORIGIN=https://*.share.example.com
```

Links then read `https://<label>.share.example.com/p/<token>/`. The label is the same for every link to one app, so what its pages keep outlasts any one link. By default it is derived from a server secret (`data/meta/app-origin.key`) rather than the app's id; give an app one that reads well, like `budget`, in its **Settings** tab or with the `label` of `deploy_app` and `manage_app`'s `update`. An app keeps the labels it had before, and their page links redirect to the new one, but visitors' browser storage stays with the hostname they used, so choose the label before sharing. Old `share.example.com/p/<token>/` page links redirect there; the backend still answers under them, without cookies, for clients that call it directly.

What each app hostname answers: that app's `/p/<token>/` links and the bundled libraries, nothing else; another app's token there is `404`. Only the app's own pages may call it, plus top-level `GET` navigations, so people can follow links to it. Other sites, and other apps' hostnames, which browsers count as the same site, get `403`, and so the app's cookies cannot be used against it. The backend's `Set-Cookie` headers lose any `Domain` attribute, so they stay on the app's hostname. Once an app is deleted, its hostname answers `Clear-Site-Data: "storage"` so visitors' browsers drop what its pages kept.

What it cannot stop: a page's own script can still set `document.cookie` with `Domain=share.example.com` or a parent domain, which other apps (or the parent domain's other sites) would then receive. Apps should name their cookies `__Host-…`, which can be set only without a domain and so cannot be shadowed. A separate registrable domain for apps closes this completely. Storage lives in each visitor's browser, per device; anything that matters, or that people share, belongs in the backend.

The wildcard must not cover the workspace's names or the share hostname. shartifacts refuses to start if it does, because those names would then serve only app links.

DNS and the edge: a wildcard record for `*.share.example.com`, and either on-demand certificates (the bundled Caddy) or a wildcard certificate (a DNS-01 challenge, or Cloudflare's edge certificate as above). The edge must pass the visitor's `Host` through for every name under the wildcard, because the label identifies the app. Chosen labels can be any lowercase DNS label, so do not match only the generated 16-character shape. Apply the same path limits as on the share hostname (`/p/` and `/vendor/`), allow WebSocket upgrades, and turn buffering off for `/p/*/api/` streams.
