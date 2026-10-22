# Cloudflare: tunnel and DNS through the API

Use this reference when the person's domain is on Cloudflare and the plan is either:

- **Cloudflare Tunnel**: no open ports. The tunnel carries the share hostname to `http://shartifacts:8081`.
- **Caddy with Cloudflare DNS**: plain DNS records (grey cloud, *DNS only*) pointing at a server whose ports 80/443 are open.

The person can do all of this in the dashboard. Doing it through the API is faster and repeatable, and it needs a token from them.

## The token

Ask the person to create an API token at *My Profile → API Tokens → Create Token → Custom token*, with:

- **Account → Cloudflare Tunnel → Edit** (only for the tunnel plan)
- **Zone → DNS → Edit**, and **Zone → Zone → Read**, limited to the one zone (*Zone Resources → Include → Specific zone*)

They put it in your shell as `CF_API_TOKEN`, or paste it. Do not write it to disk. It is not needed after setup, so suggest they delete it afterwards.

```sh
API=https://api.cloudflare.com/client/v4
AUTH="Authorization: Bearer $CF_API_TOKEN"
curl -fsS -H "$AUTH" $API/user/tokens/verify                      # "status":"active"

DOMAIN=example.com                 # the zone
SHARE=share.example.com            # the share hostname
ZONE=$(curl -fsS -H "$AUTH" "$API/zones?name=$DOMAIN" | python3 -c 'import json,sys; print(json.load(sys.stdin)["result"][0]["id"])')
ACCOUNT=$(curl -fsS -H "$AUTH" "$API/zones/$ZONE" | python3 -c 'import json,sys; print(json.load(sys.stdin)["result"]["account"]["id"])')
```

(If `python3` is missing, use `jq -r` with the same paths.)

**Before creating anything, list what exists** under that name, and do not replace a record without asking:

```sh
curl -fsS -H "$AUTH" "$API/zones/$ZONE/dns_records?name=$SHARE"
```

## Plan A: Cloudflare Tunnel

```sh
# 1. Create a remotely-managed tunnel and fetch its token.
TUNNEL=$(curl -fsS -X POST -H "$AUTH" -H 'Content-Type: application/json' \
  "$API/accounts/$ACCOUNT/cfd_tunnel" -d '{"name":"shartifacts","config_src":"cloudflare"}' \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["result"]["id"])')
TOKEN=$(curl -fsS -H "$AUTH" "$API/accounts/$ACCOUNT/cfd_tunnel/$TUNNEL/token" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["result"])')

# 2. Route the share hostname, and only it, to the public listener.
#    The paths are the ones share pages need; everything else gets a 404 at Cloudflare.
cat > /tmp/ingress.json <<EOF
{"config":{"ingress":[
  {"hostname":"$SHARE",
   "path":"^/(s|p|vendor)/|^/(runtime|bridge|icons|share|theme)\\\\.js\$|^/(view|design-system|dark)\\\\.css\$|^/favicon\\\\.svg\$",
   "service":"http://shartifacts:8081"},
  {"service":"http_status:404"}
]}}
EOF
curl -fsS -X PUT -H "$AUTH" -H 'Content-Type: application/json' \
  "$API/accounts/$ACCOUNT/cfd_tunnel/$TUNNEL/configurations" --data @/tmp/ingress.json

# 3. Point the hostname at the tunnel (proxied, orange cloud).
curl -fsS -X POST -H "$AUTH" -H 'Content-Type: application/json' "$API/zones/$ZONE/dns_records" \
  -d "{\"type\":\"CNAME\",\"name\":\"$SHARE\",\"content\":\"$TUNNEL.cfargotunnel.com\",\"proxied\":true}"
```

Run this in `bash`. The `path` in `/tmp/ingress.json` should read `\\.js`, which is JSON for the regex `\.js`. Delete the file afterwards. Then, in `.env`:

```sh
COMPOSE_FILE=compose.yaml:deploy/cloudflare-tunnel/compose.yaml
CLOUDFLARE_TUNNEL_TOKEN=<TOKEN>        # have the person paste it, or write it yourself without printing it
SHARTIFACTS_SHARE_ORIGIN=https://share.example.com
```

`docker compose up -d`, then `docker compose logs cloudflared | tail` should show registered connections.

**App hostnames.** Cloudflare's free certificate covers only `example.com` and `*.example.com`. Choose one of the options in `docs/sharing.md#cloudflare-tunnel` with the person. For a dedicated domain (share on the apex, apps one level down), add a second ingress rule before the 404 rule:

```json
{"hostname":"*.example.net","path":"^/(p|vendor)/","service":"http://shartifacts:8081"}
```

Add a proxied wildcard CNAME `*` → `$TUNNEL.cfargotunnel.com`, and set `SHARTIFACTS_APP_ORIGIN=https://*.example.net`.

**Never** add an ingress rule for the workspace's hostname or for port 8080.

## Plan B: DNS records for the bundled Caddy

The server's public IP: `curl -fsS https://api.ipify.org` (and `https://api6.ipify.org` for IPv6).

```sh
IP=203.0.113.10
for NAME in "$SHARE" "*.$SHARE"; do              # drop the wildcard if there are no app hostnames
  curl -fsS -X POST -H "$AUTH" -H 'Content-Type: application/json' "$API/zones/$ZONE/dns_records" \
    -d "{\"type\":\"A\",\"name\":\"$NAME\",\"content\":\"$IP\",\"proxied\":false}"
done
```

`proxied:false` (grey cloud) matters: Caddy gets the certificates itself, and Cloudflare's proxy would stand in the way for `*.share` names. A home connection with a changing IP needs dynamic DNS. A container such as `favonia/cloudflare-ddns` can keep both records current; ask the person before adding one.

## Undo

```sh
curl -fsS -X DELETE -H "$AUTH" "$API/zones/$ZONE/dns_records/<record id>"
curl -fsS -X DELETE -H "$AUTH" "$API/accounts/$ACCOUNT/cfd_tunnel/$TUNNEL"   # after stopping cloudflared
```

## Things to tell the person

- **Bot Fight Mode**, if it is on for the zone, can challenge visitors and link previews. The free plan cannot exempt one hostname from it.
- Cloudflare sees all traffic to the share hostname, decrypted. It never sees the workspace.
