# Apps

Apps are full-stack micro frontends. Each opens as a tab on the home page, keeps running while you switch tabs, and has every deployment as a numbered, immutable version.

**Focus.** **⤢ Focus** in an app's toolbar fills the window with the app: the same frame grows over the hub, so nothing reloads and the hub keeps its storage. A pill at the top edge names the app and tucks into a small handle after a moment; tap the handle to bring it back. Leave with the pill's **Exit**, Back, **Esc** (twice when the app has the keyboard, since the app's own menus and dialogs get the first press) or by switching tabs. `/apps/{id}?focus` opens an app focused, which makes a good bookmark or home-screen link. In the installed app (`display-mode: standalone`), where there is no browser around a page, **Open ↗** is hidden so it cannot strand you on the bare app page; Focus does that job.

**The stack is fixed**, so every app builds and deploys the same way:

| Part | What you deploy | What shartifacts does |
| --- | --- | --- |
| Frontend | Static files from the app's root: `index.html`, plus TypeScript/JSX entries a page loads Vite-style (`<script type="module" src="/src/main.tsx">`) | Bundles those entries with esbuild on deploy (React 19, automatic JSX, CSS imports, `@/` for `src/`) against the same vendored libraries artifacts use (recharts, d3, three, lucide-react, shadcn/ui…), Tailwind included. Other npm packages: build locally (`vite build --base=./`) and deploy with `"frontend": {"root": "dist"}` |
| Backend (optional) | Go: `package main` in `server/` with `go.mod`/`go.sum`. Rust or anything else: a prebuilt static binary at `server/app` | Builds Go with `go build` (CGO off; modules download at build time or come from `vendor/`), checks a prebuilt binary's OS/architecture, starts it once to prove it listens, then runs one process per version on demand and stops it when idle |
| Data | Whatever the backend writes under `$DATA_DIR` | Keeps it across versions; every version of the app shares it |

**Backend contract.** Listen on `127.0.0.1:$PORT`. Requests arrive with the app's base stripped: a page's `fetch("api/notes")` reaches the backend as `/api/notes`. Environment: `PORT`, `HOST`, `DATA_DIR`, `APP_ID`, `APP_VERSION`, `DATA_QUOTA` (bytes, when the operator sets a quota), plus the app's own variables and secrets (set in the tab's settings or with `manage_app` op `update`; values are never shown again). Headers: `X-Shartifacts-User` (`{"id","name"}` of the person on your network, absent for public visitors), `X-Shartifacts-Access` (`lan`, `public` or `public-read-only`), `X-Shartifacts-Version`, `X-Forwarded-Prefix`; clients cannot forge them. WebSockets and server-sent events pass through. Several versions may run at once against the same `DATA_DIR`, so read data per request and lock writes.

**Frontend rules.** Pages run in the same opaque-origin sandbox as artifacts. Use relative URLs (the app is served at `/run/{id}/live/`, at `/run/{id}/{n}/` for a version and at `/p/{token}/` for a public link); shartifacts injects a small runtime that adds the page's session to `fetch`, XHR, `EventSource` and `WebSocket` calls under the app's base, so there is nothing to configure. Route with hashes (the sandbox refuses `history.pushState` to other paths; unknown extensionless paths still serve `index.html`). There are no cookies, `localStorage` lasts only for the page, and pages load nothing from outside: call external services from the backend. `window.shartifacts` describes the app, version and viewer.

**Layout:**

```
index.html            frontend entry
src/main.tsx …        bundled when a page loads them
public/               served at the root (Vite's convention)
server/main.go        Go backend, never served (nor go.mod, go.sum, vendor/, *.go, *.rs, Cargo.*)
go.mod, go.sum
shartifacts.json      optional: {"frontend": {"root": "dist", "spa": true},
                                 "backend": {"go": "./server" | "binary": "server/app", "network": false}}
```

**Deploying.** In the hub: **New app** starts from the notes template (React + Go) or an archive; each tab's **Versions** panel deploys an archive, previews any version, promotes or rolls back, and downloads source. Agents use three MCP tools: `get_app` (with `guide: true` first), `deploy_app` and `manage_app` (below). Shell agents can upload a zip or tar.gz (one top-level folder is unwrapped):

```sh
curl --fail-with-body -H 'Content-Type: application/zip' --data-binary @app.zip \
  'http://localhost:8080/api/apps?name=My+App'                         # a new app
curl --fail-with-body -H 'Content-Type: application/zip' --data-binary @app.zip \
  'http://localhost:8080/api/apps/APP_ID/deployments?note=what+changed' # a new version
```

A deploy builds the version, starts its backend once, and makes it live; a failed build never goes live and returns its error and log. `promote=false` (`promote:false`) stages a version for preview instead, and `merge=true` overlays the previous version. The version that stopped being live keeps serving in-flight requests for 15 seconds, then stops (a preview starts it again). Limits: 256 MiB and 5000 files per version.

**Versions and disk.** Every deploy is a new version, so the history grows with each one. Three things keep it cheap:

- **Rollback window.** The newest 20 versions that built keep their files (`SHARTIFACTS_APP_KEEP_VERSIONS`), along with the live version and any version a link pins. Older versions stay in the history but lose their files. Failed builds don't count toward the 20, so a run of them never costs you a version to roll back to. The **Versions** panel and `get_app` (`oldest_rollback`) show the oldest version you can still roll back to.
- **Shared files.** A file that a deploy didn't change is a hard link to the previous version's copy. That holds for a full archive as much as for `merge`.
- **Reused builds.** A Go backend whose code didn't change (its packages, `go.mod`, `go.sum` and `vendor/`) reuses the previous version's binary instead of building again, and its build log says so. A prebuilt binary that didn't change is shared the same way.

A frontend-only change therefore costs about the size of the files it changed. The tab's **Settings** panel shows how much disk the app's versions and its backend data take. `SHARTIFACTS_APP_DATA_QUOTA` caps backend data per app.

**Logs.** A tab's **Logs** panel, `get_app` with `logs: true` and `GET /api/apps/{id}/logs` show the backend's stdout and stderr, starts, exits and crashes, the build log, and errors browsers hit in the app's pages.

**Public links.** With `SHARTIFACTS_SHARE_ORIGIN` set (see [sharing.md](sharing.md)), **Share** makes a link at `https://share.example.com/p/<token>/` that follows the live version or stays on one. Visitors use the whole app, frontend and backend; a **read-only** link refuses everything but `GET` and `HEAD` before it reaches the backend. On the share hostname only `/p/…` and `/s/…` are served. Revoking a link ends it at once. With `-app-origin` as well, links move to a hostname of the app's own, where the page keeps localStorage, IndexedDB and cookies (see [sharing.md](sharing.md#apps-on-hostnames-of-their-own)).

**Installing an app on its own.** Each app also installs on a phone as an app of its own, beside the workspace, with its own name and icon. Open its tab and use the browser's install (in Chrome on Android, **⋮ → Install app**): the hub links a manifest made for the app in that tab, at `/apps/{id}/manifest.webmanifest`. The installed app opens at `/apps/{id}?installed`, where the hub shows the app alone, without its tab bar or the app's toolbar. It is the signed-in workspace, not a public link: with sign-in on, it needs the same session the browser already has. The manifest is made from the app as it is, so new apps install with no change to the server, and an installed app picks up a new name or icon when the phone next checks. The icon is the first of:

- `icon.png` at the live version's web root (`public/icon.png` works), square and 192–2048 px, plus `icon-maskable.png` with its picture inside the middle 80% for launchers that crop icons to a shape;
- a tile the hub draws from the app's emoji or the first letter of its name, when its tab is opened (the browser has the emoji font; the server keeps the tile);
- the workspace's icon.

The hub page for an app (`/apps/{id}`) links that app's manifest in the HTML it is served with, so a browser that decides on a page as it loads already sees the app and not the workspace. The **⤓ Install** button in the app's toolbar uses that: on a page loaded as the app it opens the browser's install prompt, and from anywhere else it reloads the page as the app (`/apps/{id}?install`) and offers the prompt there. If the browser offers nothing (Chrome says the workspace is "already installed" when you are in the installed workspace app itself), use Chrome rather than the installed shartifacts window.

The same `https://` requirement as [installing the workspace](install.md#install-it-on-your-phone) applies.

On Android this often fails once the workspace itself is installed: `/apps/{id}` lies inside the workspace's scope (`/`), and Chrome then treats the page as part of the installed shartifacts app ("Open in shartifacts") instead of offering a second one. A [private address](#a-private-address-for-each-app) avoids that, and with one set, **⤓ Install** uses it.

**Smooth scrolling.** An app's own page scrolls on the GPU in a hub tab as it does opened on its own; the hub adds nothing to a scroll. Two things in an app's code keep it from a 120 Hz screen's rate, and the build guide tells agents to avoid both:

- **A see-through scroller.** Where text is drawn for LCD screens (Chrome on Windows and Linux, at less than 1.5× scale), the browser moves a scrolling element on the GPU only if it paints an opaque background of its own. One it can see through is re-rastered on every frame it scrolls. A page's own scroll is exempt; a panel with `overflow: auto` is not. The workspace's own scrollers (the apps home, the library, panels and drawers) each paint the colour behind them for the same reason.
- **Motion driven by script.** Chrome for Android runs a page's script, `requestAnimationFrame` and `scroll` events included, at 60 Hz on a 120 Hz screen, wherever the page is, except while a finger is on the screen: a fling after the finger lifts gets 60. The GPU's work (scrolling and compositor animations) isn't held back. Safari also runs `requestAnimationFrame` at half rate in a cross-origin frame, as app pages are in the hub, until the person taps inside it (scrolling doesn't count). So something a script moves while the page scrolls, such as a reading-progress bar fed by `scroll` events, steps at 60 Hz against content moving at 120. Native scrolling with `scroll-snap`, `position: sticky`, and CSS transitions, animations and scroll-driven animations (`animation-timeline`) of `transform` and `opacity` aren't held back either way.

### A private address for each app

With `SHARTIFACTS_PRIVATE_APP_ORIGIN=https://*.apps.example.com`, every app also answers at the root of a hostname of its own, by the same label its public links use: `https://budget.apps.example.com/`. Its pages, `api/` and the rest are there, at `/` rather than under `/run/{id}/live/`. It is private:

- **Who reaches it.** The same people as the workspace: LAN, tailnet and loopback clients, and nobody a proxy forwards from a public address (unless `SHARTIFACTS_AUTH_PROXY`). The public listener never serves it, whatever `Host` arrives. Only the app's own pages call it (as on its [public hostname](sharing.md#apps-on-hostnames-of-their-own)): other sites, other apps' hostnames and the workspace are refused.
- **Sign-in.** With [sign-in](security.md#sign-in) on, the hostname asks for a sign-in of its own, because the workspace's session cookie stays on the workspace's hostname. A browser gets one by way of the workspace: `/apps/{id}/open` (signed in there) sends it to the app's hostname with a one-time ticket good for a minute, which is swapped for a host-only `__Host-shartifacts_app` cookie. That cookie is bound to the workspace session, so signing out of the workspace, or a new password, ends it too. Opening the address without one sends the browser to the workspace (`SHARTIFACTS_PUBLIC_ORIGIN`) to sign in and back. The backend never sees the cookie.
- **What it gives the app.** An origin of its own: real `localStorage`, `IndexedDB` and cookies, apart from every other app's, and a manifest at `/_shartifacts/manifest.webmanifest` whose scope is the whole hostname, so it installs as itself whatever else is installed. Same-origin calls need no session token. `/vendor/` and `/media/` serve the bundled libraries and the workspace's media, unless the app has files there; `/_shartifacts/` is shartifacts' own.

**⤓ Install** in an app's tab, and `get_app`'s `open_url`, open the app there signed in; then use the browser's install (Chrome on Android: **⋮ → Install app**). An app's **Settings** show the address, and its **Subdomain** sets the label. Changing it moves the address: the old one redirects, but storage and an installed app stay with the old hostname, so choose the label before installing.

It needs, like the [public app hostnames](sharing.md#apps-on-hostnames-of-their-own):

1. **Wildcard DNS** for `*.apps.example.com` that only your network resolves to the server: a local DNS record (Pi-hole, your router), or a public record pointing at a private or tailnet address (`192.168.1.20`, `100.x.y.z`).
2. **An `https://` certificate** for `*.apps.example.com`, since phones install only from `https://`. Let's Encrypt issues wildcards by DNS challenge, so the hostnames never need to be reachable from the internet.
3. **A private reverse proxy** that terminates TLS for those names and forwards them, with their `Host`, to the workspace port (`8080`), never to the public listener. Keep it reachable only from your LAN or tailnet, as for the workspace.

```text
SHARTIFACTS_PUBLIC_ORIGIN=https://shartifacts.example.com
SHARTIFACTS_PRIVATE_APP_ORIGIN=https://*.apps.example.com
```

It must be a wildcard of its own: shartifacts refuses one that equals `SHARTIFACTS_APP_ORIGIN` or covers the workspace or share hostname.

**Browser storage.** App pages run with an opaque origin, so the browser gives them no storage of their own. In a hub tab, the hub keeps each app's `localStorage` for it, per app and per browser (in the hub's IndexedDB), and `sessionStorage` while the tab stays open. A page opened on its own keeps both only until it reloads. Cookies never pass to or from a backend except on an app's own origin, public or [private](#a-private-address-for-each-app), where pages keep real storage too.

## Where backends run

In the compose stack, the **runner sidecar** (`shartifacts -runner`, image target `runner`) holds the Go toolchain and runs every backend. It mounts only `data/apps`, so app code never sees artifacts, media, people or the rest of the data directory. Compose gives it its own memory and PID limits, so a runaway app cannot take shartifacts down. It is not published: shartifacts reaches it over the compose network (`SHARTIFACTS_RUNNER_URL=http://runner:8090`) with a token both share through `data/apps/.runner-token` (or `SHARTIFACTS_RUNNER_TOKEN`). Without a runner URL, as in local development or a single container, shartifacts runs backends itself. Go builds then need `go` on its `PATH`, and the image without the runner has none, so only prebuilt backends deploy there.

On Linux each backend is also confined with **Landlock**. It needs no privileges and works under Docker's default seccomp profile with `cap_drop: ALL`. A backend may read only its own build, write only its `DATA_DIR`, bind only its port, and open TCP connections only if its `shartifacts.json` sets `"backend": {"network": true}`. It cannot run other programs or signal other processes. `scripts/docker-apps-test.mjs` checks all of this from inside a deployed app. Landlock rules do not survive FUSE filesystems: Docker Desktop's shared folders and **Unraid's `/mnt/user`**. The runner tests a confined start at boot. Where that fails, it runs backends unconfined, and the hub's launcher, the build guide and its log say why. Keep `data/apps` on a local disk path (`/mnt/cache/appdata/…` on Unraid, or a named volume) to keep confinement. Landlock is a second line of defence for code you deployed yourself, not a boundary for hostile code. The real boundaries are the sidecar's container, and the opaque-origin sandbox that keeps app pages away from the workspace.

**Security model.** App pages cannot call the workspace API (opaque origin). Their calls to their own backend are cross-origin requests from `null`, which shartifacts accepts only with the HMAC-signed session it put in that page. That session is bound to the app and the URL it was served at, carries the viewer, and lasts 30 days. Requests without an `Origin` header (curl, agents) need no session while sign-in is off, which matches the trusted-network model: anyone on your network can already reach everything. With [sign-in](security.md#sign-in) on, they need the agent's token (`Authorization: Bearer shx_…`, which shartifacts removes before the request reaches the backend) or a signed-in browser. A public link's token is its own grant.
