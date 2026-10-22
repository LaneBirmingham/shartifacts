# Claude artifact compatibility and the capability runtime

shartifacts implements the runtime that Claude's artifacts assume, so source written for Claude (by you, an agent, or copied from a chat) works without edits — offline.

| Capability | How it works here |
| --- | --- |
| **React libraries** | `recharts`, `lucide-react`, `d3`, `lodash` (and `lodash/<fn>`), `papaparse`, `mathjs`, `three`, `chart.js` (and `chart.js/auto`), `xlsx` (SheetJS), `tone`, `plotly` (also `plotly.js`, `plotly.js-dist(-min)`), `highlight.js`, plus `react`, `react-dom`, `react-dom/client`. Each is a separate bundle under `/vendor/lib/`, loaded only by artifacts that import it, sharing one React instance. |
| **shadcn/ui** | `@/components/ui/*` (Button, Card, Alert, Badge, Input, Textarea, Label, Checkbox, Switch, Slider, Progress, Select, Tabs, Accordion, Collapsible, Dialog, AlertDialog, Sheet, Popover, Tooltip, DropdownMenu, RadioGroup, Table, Avatar, Separator, Skeleton, ScrollArea, Toggle) and `@/lib/utils` (`cn`). Same names, props and Tailwind classes, implemented without Radix: enough behaviour for small tools, not a full accessibility layer. |
| **Tailwind** | React artifacts that use utility classes (or any shadcn/ui import) get Tailwind's v4 browser build, with the shadcn color tokens (`bg-primary`, `text-muted-foreground`, …) and Tailwind v3's default border color. It is not added to React artifacts that do not use it, because its preflight resets unstyled elements. |
| **CDN scripts in HTML** | Classic `<script src>` tags for these libraries on cdnjs, jsDelivr, unpkg, `cdn.tailwindcss.com`, `cdn.plot.ly` or `cdn.sheetjs.com` are served from this server instead, with the library's usual global (`Chart`, `d3`, `THREE`, `_`, `Papa`, `math`, `XLSX`, `Tone`, `Plotly`, `Recharts`). The saved source is unchanged; `integrity` attributes are dropped because the local file differs. Versions are the bundled ones, not the URL's. `tailwind.config = {…}` colors and fonts are honoured. Module scripts are left alone. HTML can also load `/vendor/lib/<name>.js` directly. |
| **`window.storage`** | Claude's persistent storage API: `get(key, shared?)`, `set(key, value, shared?)`, `delete(key, shared?)`, `list(prefix?, shared?)`, string values up to 5 MiB, keys up to 200 bytes without whitespace, slashes or quotes, 20 MiB per artifact. With one user, personal and shared scopes are both simply kept. Data belongs to the artifact (all its versions), is not copied by Duplicate, and is deleted with it. Agents use `artifact_storage`. |
| **`window.fs.readFile`** | `await window.fs.readFile("sales.csv", {encoding: "utf8"})` reads a file from the Media library by the name it was uploaded with (the latest upload wins), or by its `/media/` URL. Without `encoding` it resolves to a `Uint8Array`. |
| **`window.claude.complete`** | Off by default. Start the server with `ANTHROPIC_API_KEY` set and artifacts can call `await window.claude.complete(prompt)` (or pass an array of `{role, content}` messages) to get Claude's reply text. `SHARTIFACTS_CLAUDE_MODEL` picks the model (default `claude-opus-5-5`). Requests go from this server to the Anthropic API; anyone who can reach the app can spend the key, and at most four run at once. |
| **Slide decks** | `slides` artifacts render Markdown as a 16:9 deck scaled to fit: arrow keys (also forwarded from the workspace and full-page view), Space, Home/End, clicks and swipes move between slides; `F` toggles fullscreen and `N` the speaker notes (text after a `Note:` line). A slide holding just an `h1` and a short line or two becomes a title slide. Printing gives one slide per page. |
| **Math** | Markdown and slides render `$…$` and `\(…\)` inline and `$$…$$` and `\[…\]` display math with KaTeX (fonts bundled). A `$` must touch its text, so "$5 and $10" stays text. React artifacts can `import katex`. |
| **Code highlighting** | `code` artifacts and fenced Markdown blocks are highlighted (highlight.js, common languages). A `code` artifact takes its language from its title's extension (`sync_media.py`, `App.tsx`), otherwise it is detected. |
| **Full-page view** | **Open ↗** shows `/view/{id}`: the artifact alone under a slim title bar, following new versions live (`?version=N` pins one). Storage and `window.fs` work there too. |

`window.storage`, `window.fs` and `window.claude` need the page that embeds the preview (the workspace or `/view/`): the sandboxed frame has no network access of its own, so it asks its parent page, which only answers for the artifact it is showing. A bare `/preview/{id}` URL keeps storage in memory for that page and rejects the other two.

## The capability runtime (`claude.use`)

Claude's current artifacts reach platform features through `await claude.use(name)`, which resolves a frozen namespace or `null` (design for absence). shartifacts implements that runtime (contract 0.2.84) for every artifact, with no manifest needed: everything is granted on this trusted network, and `permissions.state()` says so.

| Capability | Here |
| --- | --- |
| `user` | The person using this browser (see [People](#people)): `me()`, `id()`, `isOwner()` (who created the artifact; everyone, for artifacts an agent made), `canEdit()` (true: anyone on the network may edit), `can()`, `profiles(ids)` and `search(q)` over everyone who has given a name. |
| `db` | A realtime document store per artifact, Firestore-shaped as Claude's: `doc`/`collection` refs with Claude's path grammar, `get`/`set`/`update`/`delete`, `where`/`orderBy`/`limit` queries, `onSnapshot` live across every open view (server-sent events) with `docChanges()`, cooperative `acquire` leases, 256 KiB documents and 25,000 per artifact. Each person's `data/users/<id>/` subtree is private, from the owner too, and `capabilities.db.rules` declared on the artifact (`{path, read, write}` with `view`/`interact`/`admin`/`owner` and `{self}` subtrees) set access as Claude's do; on this network everyone holds `admin`, and the creator `owner`. Agents read and write it with `artifact_db`. |
| `assets` | `upload(blob)` stores images, SVG, video, PDF, fonts, CSS/JS and CSV/Markdown/JSON/text for the artifact (20 MiB each; 2 MiB SVG; 1,000 files and 1 GiB per artifact), served at `/_blob/<id>`; `list()` and `delete(id)`. |
| `artifact` (and `self`) | `publish(html)` replaces the page with a new version; `publish({path: content \| null})` changes files and keeps the publishing view running. Compare-and-set against the version the view runs, or per file with `ifMatch` sha256s, rejecting `conflict` with the paths that moved. Other views follow within seconds. `metadata(refs)` gives other artifacts' titles. `edit`/`sync` belong to Claude's live docs and reject `invalid_content`. |
| `comments` | `create`/`reply`/`resolve`/`delete` write to the artifact's comment threads as the viewer; `openComposer` opens the workspace's comment box with what the page pointed at; `anchorFor` builds anchors; `sendToClaude` posts a thread marked for your agent (`to_claude`, seen by `artifact_comments` and `watch_artifacts`). |
| `room` | Everyone viewing the artifact right now: `presence()`/`peers()`/`onPeers()` and `emit()`/`on()` topic messages, named rooms with `join()`, `connected()`/`onConnection()`. `sendToClaudeSession(data)` leaves the data for your agent as a `to_claude` comment. Nothing is stored. |
| `sample` | Ask Claude, when the server has `ANTHROPIC_API_KEY` (otherwise `use("sample")` is `null`): `sample(input, options)` and `sample.json()`, streaming through `onText`, `signal` to stop, `images`, page `tools` the bridge runs between rounds, `modelTier` (`quick` = Claude Haiku 5.5, `default` = `SHARTIFACTS_CLAUDE_MODEL`, `complex` = Claude Opus 5.5), `model`, `effort`, and five-minute answer caching. |
| `mcp` | `listTools`, `callTool`, `watchTool`, `invalidate`, `server(name)` and `describeTool` over the MCP servers listed in `SHARTIFACTS_CONNECTORS` (their credentials stay on the server), plus `artifacts_data`, this artifact's own `db`. |
| `files` | The Media library, read as project files: `list`, `read`, `download`, and `asSampleTools()` for `sample`. |
| `downloads` | `save({filename, data})` asks you, then saves; Claude's extension allowlist applies. |
| `permissions` | `state`/`request` report every capability `granted`; `manage` rejects `unavailable` (the operator decides here). |

Gallery thumbnails get a read-only runtime: they read data but their writes are not saved, and `sample`, `room` and uploads are off. A preview opened on its own (`/preview/{id}`) resolves every `use()` to `null`.

### People

There are no accounts (the optional owner password is one for everyone). **Who are you?** in the top bar lets each person pick or add their name (and colour); the browser remembers it. That identity is what artifacts see through `user`, keys each person's private `data/users/<id>/` documents and `room` presence, records who created an artifact, and signs comments. Anyone who can use the workspace can pick any name: it keeps people's data apart, it is not authentication.

### Connectors

Set `SHARTIFACTS_CONNECTORS` (`-connectors`) to a JSON file listing the MCP servers (Streamable HTTP) artifacts may call through `claude.use("mcp")`:

```json
[{ "name": "GitHub", "url": "https://example.com/mcp", "headers": { "Authorization": "Bearer …" } }]
```

Calls run from the server, so tokens never reach a page; anyone who can reach the app can use them, as with `ANTHROPIC_API_KEY`. With Docker, put the file in the data volume and set `SHARTIFACTS_CONNECTORS=/data/connectors.json`.
