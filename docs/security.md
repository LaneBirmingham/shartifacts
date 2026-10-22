# Security model

shartifacts is a single-household tool. It deliberately has no accounts or passwords: **who can reach the workspace is the whole of its access control.** This page explains what that protects, what it does not, and how to keep it that way. To report a vulnerability, see [SECURITY.md](../SECURITY.md).

## Trust boundaries

| Who | What they can do |
| --- | --- |
| Anyone who reaches the **workspace** (port 8080: this machine, your LAN, your tailnet) | Everything: read, change and delete every artifact, app, media file and setting. Deploy app code, which then runs on your server. Spend `ANTHROPIC_API_KEY` and use connectors. |
| Anyone with a **share link** | Read that one artifact (and the data its page shows), or use that one app as its link allows. Nothing else. |
| Anyone else on the internet | Nothing. With sharing on, they can reach the public listener, but it only returns `404` without a valid token. |

People pick a name under **Who are you?**. That name keeps their private documents apart and signs their comments, but it is not authentication: anyone on the network can pick any name.

## Keeping the workspace private

These checks run on every workspace request:

1. **The peer must be private.** The direct connection must come from loopback, a private range (RFC 1918, IPv6 ULA) or Tailscale's `100.64.0.0/10`. Public peers get `403`.
2. **Forwarded clients must be private too.** A reverse proxy hides the real client, so shartifacts reads the forwarding headers proxies add (`X-Forwarded-For`, `X-Real-IP`, `Forwarded`, `CF-Connecting-IP`, `True-Client-IP` and similar). If any of them names a public address, the request is refused. Forwarded headers are never used to *grant* access, because anything that reaches the port can write them. They are used only to *deny* it, which costs a forger nothing but their own request. See [Behind a reverse proxy](#behind-a-reverse-proxy).
3. **The Host must be one of yours**: a LAN IP, `localhost`, `SHARTIFACTS_PUBLIC_ORIGIN`'s hostname, or one listed in `SHARTIFACTS_ALLOWED_HOSTS`. This defeats DNS rebinding, where a public website makes your browser talk to your LAN under the website's own name.
4. **No cross-site requests.** API and MCP calls with a foreign `Origin` are refused, and so are cross-site requests marked by Fetch Metadata (`Sec-Fetch-Site: cross-site`). The exceptions are top-level navigations (following a link) and the read-only files sandboxed previews load. A malicious page you visit cannot drive your workspace.

### Behind a reverse proxy

A proxy on the same machine (Caddy, nginx, `tailscale serve`) connects from a private address, so check 1 always passes for it. If that proxy also answers the workspace's hostname from the internet, the second check refuses those requests, as long as the proxy adds a forwarding header naming the client. Caddy, Traefik, cloudflared, Nginx Proxy Manager, Apache and Tailscale all add one. Stock nginx does not, unless you set `proxy_set_header X-Forwarded-For $remote_addr;`. The examples in [reverse-proxies.md](reverse-proxies.md) include it.

The real fix is to never route the workspace from the internet at all. Restrict the proxy to your networks (an nginx `allow`/`deny`, an NPM Access List, or a tailnet-only DNS name).

If you put the workspace behind a proxy that **authenticates every request** itself (Cloudflare Access, Authelia, oauth2-proxy), set `SHARTIFACTS_AUTH_PROXY=true` so shartifacts accepts the public clients it forwards. You are then trusting that proxy completely. Sandboxed previews load scripts without your login cookie unless the proxy's session cookie is `SameSite=None`, so test previews before relying on this. The project does not test this setup.

### Deployment pitfalls

- **Never forward port 8080 from your router**, and never `tailscale funnel` it. Share publicly with `SHARTIFACTS_SHARE_ORIGIN` instead.
- **On a VPS, keep `SHARTIFACTS_BIND=127.0.0.1`.** Rootless Docker, Podman and some cloud load balancers rewrite the client's address to a private one, which would make every visitor look local. Reach the workspace through Tailscale or an SSH tunnel.
- **Docker Desktop** (macOS, Windows) also shows every client as private. That is fine on a home network behind NAT, and a reason never to forward its port.
- **Home networks are trusted.** Anyone on your Wi-Fi, guests and smart devices included, can use the workspace if it is bound to `0.0.0.0`. Bind it to `127.0.0.1` and use Tailscale if that is not what you want.

## Public sharing

- **The public listener** (port 8081, on only when sharing is) serves only share links, app links and the static files their pages load. It answers anything else with `404`, whatever `Host` a proxy sends, so a misconfigured edge cannot reach the workspace through it. Edges should forward there. Port 8080 also answers the share hostname by `Host`, for compatibility.
- **Tokens** are 128-bit random values. Every read on the share hostname is resolved through a token, so token A reads nothing of artifact B. Workspace URLs such as `/media/` and `/_blob/` return `404` there. Shared pages get their media rewritten under their token.
- **Read-only** artifact links can read shared data but never write it. Read-only app links pass only `GET` and `HEAD` to the backend, with no WebSocket upgrades. Revoking a link ends it at once, and its open streams close within seconds.
- **Bounded work for strangers**: a share link's data calls read a body of at most 64 KiB, and only after the token is found. React builds are cached, so reloading a page does not rebuild it.
- **Certificates**: the bundled Caddy asks `GET /api/tls-allowed` before getting a certificate. That endpoint approves only the share hostname and the hostnames of apps that exist, so the workspace never gets a public certificate and strangers cannot use up your Let's Encrypt limits.
- **App hostnames** (`SHARTIFACTS_APP_ORIGIN`) answer only their own app's links. Requests from other sites, and from other apps' hostnames (which browsers count as the same site), are refused. Backend cookies lose their `Domain`, so they stay on the app's hostname. A page's own script can still set a cookie on the parent domain, so apps should use `__Host-` cookies. A separate registrable domain for apps removes the problem entirely.

## Sandboxing what runs

**Artifacts** render in an iframe with an opaque origin (`sandbox` without `allow-same-origin`), and the same restrictions apply through the response's CSP when a preview URL is opened directly. They cannot read the workspace, its storage or cookies. They cannot fetch the API or external URLs, submit forms anywhere, or embed frames. They load only the bundled libraries and your media. Their bridge to `window.storage`, `claude.use(…)` and friends goes through the embedding page, which answers only for the artifact it shows. Clicking a link opens a new tab outside the sandbox. The sandbox does not limit CPU or memory: a deliberately expensive artifact can still slow the browser tab it runs in.

**App pages** run in the same opaque-origin sandbox, except on their own hostnames. Their calls to their own backend are accepted only with an HMAC-signed session that shartifacts embedded in the page. The session is bound to the app and the URL, and lasts 30 days. Requests with no `Origin` (curl, agents) need no session, which matches the rest of the network model.

**App backends** are code you or your agent deployed, and they run on your server. In the Compose stack they run in the **runner sidecar**, which mounts only `data/apps`. It cannot see artifacts, media, people or the rest of the data folder, and it has its own memory and PID limits. On Linux each backend is also confined with **Landlock**. A backend can read only its own build, write only its `DATA_DIR`, and bind only its port. It can open outbound connections only if its manifest asks for network access, and it cannot run other programs. Landlock needs a local filesystem: FUSE mounts such as Unraid's `/mnt/user` and Docker Desktop's shared folders lose its rules, and the runner then says so in its log and in the UI. Treat Landlock as a second line of defence for your own code, not a boundary for hostile code. Do not deploy apps you would not run on the machine yourself.

The runner is reachable only on the Compose network, with a token that it and shartifacts read from the apps folder. shartifacts sets `X-Shartifacts-*` and `X-Forwarded-*` headers for backends itself, after removing any that a client sent (including through `Connection`), so backends can trust them.

## Secrets and data

- `ANTHROPIC_API_KEY` and connector credentials stay on the server and never reach a page. Anyone who can reach the workspace can still *use* them through artifacts.
- App environment variables and secrets are write-only in the UI and API. Backends receive them as environment variables.
- Data is stored as plain files without encryption. Use disk encryption if you need it, and protect your backups the same way.
- shartifacts makes no outbound connections of its own, except to the Anthropic API when a key is set, to connectors you configured, and (in the runner) to the Go module proxy while building app backends.

## Checklist

- [ ] The workspace does not answer from outside your network: test from a phone with Wi-Fi off.
- [ ] `SHARTIFACTS_BIND` is `127.0.0.1` on a VPS, and port 8080 is forwarded from nowhere.
- [ ] If sharing is on, the edge forwards only the share hostname (and the app hostnames), to port 8081.
- [ ] `.env` is readable only by you (`chmod 600 .env`).
- [ ] The data folder is backed up, and the backups are protected like the data.
- [ ] You update regularly: `git pull && docker compose up -d --build`.
