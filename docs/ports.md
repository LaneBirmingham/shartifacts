# Port registry

Agents that deploy things around your network — a Docker stack on the server, a dev server on a laptop — pick ports blind, and two of them end up on the same one. shartifacts is the place they all connect to, so it keeps the list: which ports services use on each machine, who looks after them, and why. An agent checks it before it publishes a port and claims one; a claim that would collide is refused with who holds the port and the next free one.

The registry is a ledger, not a scan. shartifacts cannot see other machines, so it knows only what agents and people claimed. Seed it with what already runs (below), and keep checking the machine itself when you can (`ss -ltnup`, `docker ps`): something nobody registered can still hold a port.

**App backends never need it.** shartifacts gives each backend its own free loopback port as `$PORT`, inside the runner, so they cannot collide with each other or with anything on the host.

## In the hub

**Ports in use**, on the apps home page, lists every claim by machine: the port (and `/udp` for UDP), the service and its note, who looks after it and when it last changed. **×** releases a claim once the service no longer listens there.

## For agents

The `ports` tool, with `op`:

| `op` | Fields | What it does |
| --- | --- | --- |
| `list` (default) | optional `host` | The claims, by host, port and protocol, and `hosts`: every machine name in use. Reuse one of those rather than naming a machine twice (`myserver` and `192.168.1.20` are two hosts as far as the registry knows) |
| `claim` | `host`, `service`, and `port` or `from`; optional `protocol` (`tcp`, the default, or `udp`), `owner`, `note` | Claims the port. Without `port`, takes the lowest free one at or above `from` (default 1024). Check and claim happen together, so two agents asking at once get different ports. Claiming again with the same `service` updates the claim and keeps the owner and note it leaves out; a port another service holds is refused with its holder and the next free port |
| `release` | `host`, `port`, optional `protocol` | Ends the claim, once the service is gone |

`owner` defaults to the name of the agent's token when [sign-in](security.md#sign-in) is on, so a claim says which agent made it. The server's instructions tell every connected agent to claim before it publishes a port, so a prompt such as *"deploy Grafana on my server with Docker"* is enough.

## HTTP API

```sh
curl http://192.168.1.20:8080/api/ports                     # everything; ?host=myserver for one machine
curl -H 'Content-Type: application/json' http://192.168.1.20:8080/api/ports \
  -d '{"host":"myserver","from":8100,"service":"grafana","note":"dashboards"}'
curl -X DELETE 'http://192.168.1.20:8080/api/ports/myserver/8100?protocol=tcp'
```

`POST` answers `201` for a new claim and `200` for an update. A taken port is `409` with `code: "port_taken"`, the `holder` and `next_free`, so a script can move on without parsing the message. With sign-in on, send a token (`-H "Authorization: Bearer shx_…"`).

## Seeding from Docker

Run this on each Docker host to claim every port its containers publish, named after the container. It is safe to run again: a port the same container already holds is updated, and one another service holds is reported and left alone. Containers on the host network publish nothing for Docker to list, so claim theirs by hand, as with anything outside Docker.

```sh
SHARTIFACTS=http://192.168.1.20:8080 HOST=myserver
docker ps --format '{{.Names}} {{.Ports}}' | while read -r name ports; do
  echo "$ports" | grep -oE ':[0-9]+->[0-9]+/(tcp|udp)' | sort -u | while read -r p; do
    port=${p#:}; port=${port%%-*}; proto=${p##*/}
    curl -sS -H 'Content-Type: application/json' ${SHARTIFACTS_TOKEN:+-H "Authorization: Bearer $SHARTIFACTS_TOKEN"} \
      "$SHARTIFACTS/api/ports" -d "{\"host\":\"$HOST\",\"port\":$port,\"protocol\":\"$proto\",\"service\":\"$name\",\"owner\":\"docker\"}"
    echo
  done
done
```

Port ranges (`6881-6889->6881-6889/tcp`) are skipped; claim the ports of a range one by one if agents should keep clear of them.

## Limits

Up to 5000 claims. Hosts are hostnames, IP addresses or short aliases (letters, digits, `.`, `:`, `-`, `_`; lowercased). Services and owners take up to 100 bytes, notes 500. The registry is `meta/ports.json` in the data folder, and backs up with it.
