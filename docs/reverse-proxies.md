# Reverse proxies

shartifacts listens on two ports, and a proxy needs to know which one it is talking to:

| Port | Serves | Who may reach it |
| --- | --- | --- |
| **8080**, the workspace | The UI, `/api`, `/mcp`, previews and media. Everything. | Only you: this machine, your LAN, your tailnet. **No login**, so never route it from the internet. |
| **8081**, the public listener (only with `SHARTIFACTS_SHARE_ORIGIN`) | Share links (`/s/…`), public app links (`/p/…`) and the files they load. Nothing else, whatever `Host` arrives. | Anyone. This is what your edge forwards to. |

Each port has its own proxy rules:

- **For the workspace** (8080), a TLS-terminating proxy is optional. It gives you an `https://` address, which artifacts need for the camera, microphone and clipboard. Set `SHARTIFACTS_PUBLIC_ORIGIN` to the address people use; without it the page loads but its API calls are refused, because the proxy forwards plain HTTP. Keep the proxy reachable only from your LAN or tailnet. If it forwards a request from a public address anyway, shartifacts refuses it unless you set `SHARTIFACTS_AUTH_PROXY` (see [security.md](security.md#behind-a-reverse-proxy)).
- **For public sharing** (8081), the edge terminates TLS for the share hostname (and the app hostnames), passes the visitor's `Host` through, does not buffer event streams, and allows WebSocket upgrades. The bundled [Caddy](sharing.md#caddy-bundled) and [Cloudflare Tunnel](sharing.md#cloudflare-tunnel) edges are already set up this way. The rest of this page covers proxies you run yourself.

## tailscale serve

The simplest way to give the workspace HTTPS. You need no domain, and it stays on your tailnet.

1. Install Tailscale on the machine running shartifacts and log in (`tailscale up`).
2. In the Tailscale admin console, under *DNS*, enable **MagicDNS** and **HTTPS Certificates**.
3. Serve the workspace port on the tailnet:

   ```sh
   tailscale serve --bg 8080
   tailscale serve status        # shows https://myserver.tailnet-name.ts.net
   ```

4. In `.env`, set `SHARTIFACTS_PUBLIC_ORIGIN=https://myserver.tailnet-name.ts.net` and run `docker compose up -d`. You can keep `SHARTIFACTS_BIND=127.0.0.1`, because `tailscale serve` connects from this machine.

Agents then connect to `https://myserver.tailnet-name.ts.net/mcp` from any of your devices. **Never** run `tailscale funnel` for port 8080: Funnel publishes it to the internet.

## nginx

The workspace, on a name that only your LAN or tailnet resolves to (for example a DNS record pointing at a tailnet IP, with a certificate from a DNS-01 challenge):

```nginx
server {
    listen 443 ssl;
    server_name shartifacts.example.com;
    # ssl_certificate …; ssl_certificate_key …;

    # Only your own networks. shartifacts also refuses forwarded public clients.
    allow 192.168.0.0/16; allow 10.0.0.0/8; allow 172.16.0.0/12; allow 100.64.0.0/10;
    deny all;

    client_max_body_size 300m;          # media uploads and app archives
    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $remote_addr;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
        proxy_buffering off;            # live updates are server-sent events
        proxy_read_timeout 1h;
    }
}
```

with `SHARTIFACTS_PUBLIC_ORIGIN=https://shartifacts.example.com`. The `Connection` value comes from the usual `map $http_upgrade $connection_upgrade { default upgrade; '' close; }` in the `http` block.

Public sharing, with the share hostname and the app hostnames:

```nginx
server {
    listen 443 ssl;
    server_name share.example.com ~^[a-z0-9-]+\.share\.example\.com$;
    # A certificate for share.example.com and *.share.example.com (wildcards need DNS-01).

    client_max_body_size 64m;
    location ~ ^/(s|p|vendor)/ { include shartifacts-public.conf; }
    location ~ ^/(runtime|bridge|icons|share|theme)\.js$ { include shartifacts-public.conf; }
    location ~ ^/(view|design-system|dark)\.css$ { include shartifacts-public.conf; }
    location = /favicon.svg { include shartifacts-public.conf; }
    location / { return 404; }
}
```

with `shartifacts-public.conf`:

```nginx
proxy_pass http://127.0.0.1:8081;       # the public listener, never 8080
proxy_http_version 1.1;
proxy_set_header Host $host;            # the app's label is in the hostname
proxy_set_header X-Forwarded-For $remote_addr;
proxy_set_header Upgrade $http_upgrade;
proxy_set_header Connection $connection_upgrade;
proxy_buffering off;
proxy_read_timeout 1h;
```

The `server_name` regex must match any label, because apps can choose labels like `budget`. Do not limit it to the generated 16-character shape.

## Nginx Proxy Manager

Add a **Proxy Host** for `share.example.com`:

- Forward to the shartifacts machine's IP, port **8081**, scheme `http`. NPM runs in its own container, so publish the public listener where it can reach it: `SHARTIFACTS_SHARE_BIND=0.0.0.0`, or the host's LAN IP.
- Turn on *Websockets Support*, *Block Common Exploits* and an SSL certificate.
- Under *Advanced*, add `proxy_buffering off;`.

For app hostnames, add another proxy host for `*.share.example.com` with the same settings and a wildcard certificate (*DNS Challenge* in NPM's SSL tab).

Do not create a public proxy host for the workspace. To give the workspace HTTPS through NPM as well, give its proxy host an *Access List* that allows only your LAN and tailnet ranges, and set `SHARTIFACTS_PUBLIC_ORIGIN`.

## Traefik

With shartifacts in the same Docker network as Traefik, add labels to the `shartifacts` service (in a `compose.override.yaml`):

```yaml
services:
  shartifacts:
    labels:
      - traefik.enable=true
      - traefik.http.routers.shartifacts-share.rule=Host(`share.example.com`) || HostRegexp(`^[a-z0-9-]+\.share\.example\.com$`)
      - traefik.http.routers.shartifacts-share.entrypoints=websecure
      - traefik.http.routers.shartifacts-share.tls.certresolver=letsencrypt
      - traefik.http.routers.shartifacts-share.service=shartifacts-share
      - traefik.http.services.shartifacts-share.loadbalancer.server.port=8081
```

Traefik passes `Host` through and streams responses by default. Wildcard certificates need a DNS challenge resolver. Do not add a public router for port 8080.

## Caddy you already run

Use the bundled [`deploy/caddy/Caddyfile`](../deploy/caddy/Caddyfile) as your site block. It needs no hostnames: it uses on-demand certificates and asks shartifacts which names to serve. Replace `shartifacts:8080` and `shartifacts:8081` with the addresses your Caddy reaches them on.

## Checking a proxy

From inside your network:

```sh
curl -s -o /dev/null -w '%{http_code}\n' https://shartifacts.example.com/api/artifacts   # 200
```

From outside your network, for example on a phone with Wi-Fi off:

```sh
curl -s -o /dev/null -w '%{http_code}\n' https://shartifacts.example.com/   # must fail or 403
curl -s -o /dev/null -w '%{http_code}\n' https://share.example.com/         # 404
curl -s -o /dev/null -w '%{http_code}\n' https://share.example.com/api/artifacts   # 404
```

Then open a real share link in a browser. A `curl` 200 shows that the socket works, not that the page does.
