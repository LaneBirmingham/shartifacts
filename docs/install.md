# Install and operate

The easiest way to install is to [let your agent do it](../README.md#set-up-with-your-agent). This page covers the same steps by hand.

## With Docker Compose (recommended)

Requirements: Docker with the Compose plugin (`docker compose version`), about 1 GB of disk for the images, and internet access while building. Once built, shartifacts runs offline.

```sh
git clone https://github.com/LaneBirmingham/shartifacts.git
cd shartifacts
cp .env.example .env            # optional; edit to taste (see modes.md)
mkdir -p data && sudo chown -R 10001:10001 data   # Linux: the containers run as UID 10001
docker compose up -d --build
docker compose ps               # both containers "healthy" / "running"
docker compose logs shartifacts | head   # the first lines name the mode
```

Then open `http://localhost:8080` (or `http://<this machine's LAN IP>:8080` if you set `SHARTIFACTS_BIND`), and [connect your agent](agents.md).

If anyone or anything you don't trust shares your network, set an owner password under **Sign-in** in the workspace (or `docker compose exec shartifacts shartifacts auth set-password`). Browsers then sign in once, and agents connect with a token that **Connect an agent** makes for you. [security.md](security.md#sign-in) explains it.

Compose runs two containers:

- **`shartifacts`** is the workspace, MCP server and share pages.
- **`shartifacts-runner`** builds and runs app backends. It has the Go toolchain, mounts only `data/apps`, is never published, and has its own memory limit.

The first build takes a few minutes. It downloads Node and Go images and builds the frontend libraries and the binary. Later builds use the cache.

## Without Docker

You need Go 1.26 or newer. The bundled browser libraries are checked in, so Node is needed only if you change them.

```sh
go build -trimpath -o bin/shartifacts .
./bin/shartifacts -data ./data              # http://localhost:8080
./bin/shartifacts -help                     # every flag, and its SHARTIFACTS_* variable
```

Without the runner sidecar, app backends run beside shartifacts, so Go backends build with the `go` on your `PATH`. On Linux they are still confined with Landlock.

To keep it running with systemd:

```ini
# /etc/systemd/system/shartifacts.service
[Unit]
Description=shartifacts
After=network-online.target
Wants=network-online.target

[Service]
User=shartifacts
WorkingDirectory=/opt/shartifacts
EnvironmentFile=-/opt/shartifacts/.env
ExecStart=/opt/shartifacts/bin/shartifacts
Restart=on-failure
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/opt/shartifacts/data

[Install]
WantedBy=multi-user.target
```

The `.env` here uses the program's own variables (`SHARTIFACTS_LISTEN=127.0.0.1:8080`, `SHARTIFACTS_DATA=/opt/shartifacts/data`, …). [configuration.md](configuration.md) lists them.

## Upgrade

```sh
cd shartifacts
git pull
docker compose up -d --build
```

Read [CHANGELOG.md](../CHANGELOG.md) first for anything that needs a change in `.env`. Your data folder carries over: its format only ever moves forward.

## Back up and restore

Everything shartifacts keeps is in the data folder (`SHARTIFACTS_DATA`, `./data` by default):

| Path | Holds |
| --- | --- |
| `<id>.json` (one per artifact, at the top) | Artifacts with every revision, description, tags and owner |
| `db/`, `storage/`, `assets/`, `comments/` | Each artifact's documents, `window.storage`, uploads and comment threads |
| `media/` | The Media library |
| `apps/` | Apps: every version's source and build, their data (`apps/<id>/data/`) and settings |
| `meta/` | People, share links, server secrets (session and label keys) and, with sign-in on, `auth.json`: the password hash and the digests of sessions and tokens |

For a consistent snapshot, stop the stack briefly:

```sh
docker compose stop
tar czf shartifacts-$(date +%F).tgz data
docker compose start
```

To restore, stop the stack, put the folder back in place (owned by UID 10001), and start it. Run exactly one shartifacts per data folder.

Files are written to a temporary file, synced, and renamed into place, so a crash never leaves half a file. shartifacts refuses to start on a corrupt file rather than silently replacing it. Revision history grows forever, and media is never deleted automatically, so that old versions keep working. Check disk use now and then.

## Platform notes

**Linux server or VPS.** These are the reference platforms. Keep `SHARTIFACTS_BIND=127.0.0.1` on a VPS ([modes.md](modes.md#on-a-vps)). Docker's published ports bypass `ufw`, which is one more reason to bind to loopback.

**Unraid.** Use the *Compose Manager* plugin, or the command line, with the repository in a share such as `/mnt/user/appdata/shartifacts-src`:

- Point `SHARTIFACTS_DATA` at a path on the **cache pool**, `/mnt/cache/appdata/shartifacts/data`, not `/mnt/user/…`. `/mnt/user` is a FUSE filesystem, on which Landlock cannot confine app backends. They still run, isolated in the runner container, and the runner's log says so.
- `chown -R 10001:10001` the data folder before the first start.
- Appdata backups then include the data folder.

**Synology, QNAP and other NAS devices.** Use Container Manager or Compose over SSH. Use a data folder on a normal volume, owned by UID 10001.

**Raspberry Pi and other ARM64 boards.** Both images build natively on `arm64`. A Pi 4 or 5 with 4 GB RAM is plenty. Expect the first build to take 10 to 15 minutes.

**macOS and Windows (Docker Desktop).** These work well for trying it out and for a single computer. Docker Desktop's shared folders are FUSE-like, so app backends run unconfined (still in their container). Docker Desktop also shows every client as private, so never expose its port beyond your machine or LAN.

## Troubleshooting

| Symptom | Cause and fix |
| --- | --- |
| `host not allowed` | You opened it by a hostname shartifacts does not know. Add it to `SHARTIFACTS_ALLOWED_HOSTS`, or use the LAN IP. |
| `local network access only` | The client's address is public, or a proxy forwarded a public client to the workspace. Keep the workspace on your LAN or tailnet ([security.md](security.md#behind-a-reverse-proxy)). |
| The page loads behind an HTTPS proxy, but the library is empty | Set `SHARTIFACTS_PUBLIC_ORIGIN` to the `https://` address you use. |
| The login page appears, and you have lost the password | On the server: `docker compose exec shartifacts shartifacts auth set-password` sets a new one, or `… auth off` turns sign-in off. |
| An agent gets `401 unauthenticated` | Sign-in is on: give the agent a token ([agents.md](agents.md#with-sign-in-on)). A revoked token gets the same answer. |
| `permission denied` on start | The data folder is not writable by UID 10001: `sudo chown -R 10001:10001 data`. |
| Can't reach it from another device | `SHARTIFACTS_BIND` is `127.0.0.1` (the default). Set it to `0.0.0.0` or the LAN IP, and check the host firewall. |
| Apps: "app runner unreachable" | The runner starts after shartifacts is healthy. Wait a minute, then run `docker compose logs runner`. |
| Apps: the sandbox says "None" or "unconfined" | Not Linux, or the data folder is on FUSE (Unraid `/mnt/user`, Docker Desktop). See the platform notes. |
| Share link opens a `404` page from your proxy | The edge does not forward that path. Allow `/s/`, `/p/` and `/vendor/` ([sharing.md](sharing.md#verify)). |
| Refuses to start with an origin error | The settings conflict, for example the share hostname is also a workspace hostname. The message says which. |

## Uninstall

```sh
docker compose down --rmi local     # add -v to delete the Caddy certificate volume too
```

Your data folder stays. Delete it yourself if you mean to.
