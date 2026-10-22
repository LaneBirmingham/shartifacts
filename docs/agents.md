# Agents and MCP

shartifacts speaks MCP over Streamable HTTP at `/mcp` on the workspace address, such as `http://192.168.1.20:8080/mcp`, or `https://myserver.tailnet-name.ts.net/mcp` behind `tailscale serve`. **Connect an agent** in the web UI shows the address of your server and the commands below, ready to copy.

The agent has to reach the workspace the way a browser does: from the same machine, your network or your tailnet. Hosted agents that connect from a provider's cloud, such as claude.ai custom connectors, cannot reach a private server.

## Client setup

Replace the URL with your server's.

**Claude Code**

```sh
claude mcp add --transport http --scope user shartifacts http://192.168.1.20:8080/mcp
```

**Codex CLI**

```sh
codex mcp add shartifacts --url http://192.168.1.20:8080/mcp
```

or in `~/.codex/config.toml`:

```toml
[mcp_servers.shartifacts]
url = "http://192.168.1.20:8080/mcp"
```

**Cursor**: `~/.cursor/mcp.json` (or `.cursor/mcp.json` in a project)

```json
{ "mcpServers": { "shartifacts": { "url": "http://192.168.1.20:8080/mcp" } } }
```

**VS Code (Copilot agent mode)**: `.vscode/mcp.json`

```json
{ "servers": { "shartifacts": { "type": "http", "url": "http://192.168.1.20:8080/mcp" } } }
```

**Gemini CLI**: `~/.gemini/settings.json`

```json
{ "mcpServers": { "shartifacts": { "httpUrl": "http://192.168.1.20:8080/mcp" } } }
```

**Claude Desktop and other clients that run only local (stdio) servers** can bridge with [`mcp-remote`](https://www.npmjs.com/package/mcp-remote):

```json
{ "mcpServers": { "shartifacts": { "command": "npx", "args": ["-y", "mcp-remote", "http://192.168.1.20:8080/mcp", "--allow-http"] } } }
```

**Anything else**: give the client a remote MCP server, transport **Streamable HTTP** (sometimes called just "HTTP"), with the URL above. Most `mcpServers` files accept `{"type": "http", "url": "…"}`.

### With sign-in on

Once the workspace has an owner password ([security.md](security.md#sign-in)), agents need a token too, sent as `Authorization: Bearer shx_…`. Without one, `/mcp` answers `401` with a message saying how to get one. Make a token per agent, so you can revoke one without touching the others:

- **Connect an agent** asks for a name, makes the token and fills it into the commands, or
- **Sign-in** in the workspace lists, makes and revokes tokens, or
- on the server: `shartifacts auth token "Codex on my laptop"` (with Docker, `docker compose exec shartifacts shartifacts auth token NAME`) prints one.

A token is shown only once. Then add it to the client:

```sh
claude mcp add --transport http --scope user shartifacts http://192.168.1.20:8080/mcp \
  --header "Authorization: Bearer shx_…"                                   # Claude Code

export SHARTIFACTS_TOKEN=shx_…        # in your shell profile too, so Codex always has it
codex mcp add shartifacts --url http://192.168.1.20:8080/mcp --bearer-token-env-var SHARTIFACTS_TOKEN
```

In configuration files, add the header next to the URL: `"headers": {"Authorization": "Bearer shx_…"}` for Cursor, VS Code and Gemini CLI, and `bearer_token_env_var = "SHARTIFACTS_TOKEN"` in Codex's `config.toml`. With `mcp-remote`, add `"--header", "Authorization:${AUTH_HEADER}"` to `args` and `"env": {"AUTH_HEADER": "Bearer shx_…"}`.

Tokens can do everything in the workspace except manage the password and tokens. Media upload URLs that tools hand out to an agent's shell carry a signed grant instead of the token, which lasts an hour, or until the token is revoked; `list_media` gives a fresh one. When an agent calls an app's backend with its token (`curl …/run/ID/live/api/…`), shartifacts removes the token before the backend sees the request.

The server uses the official Go MCP SDK in stateless JSON-response mode. No SSE subscription or session ID is needed (only a token, once sign-in is on), and it accepts the current protocol and older SDK-negotiated initializations. When it starts, an agent receives instructions describing artifacts, media uploads, apps and the design system, so a prompt such as *"make me a packing checklist in shartifacts"* is enough.

Links that tools hand out for people use `SHARTIFACTS_PUBLIC_ORIGIN` when it is set, so an agent connected by LAN address still gives you `https://…` links you can open anywhere on your tailnet.

## Tools


| Tool                 | Input                                                                                              | Result                                                               |
| -------------------- | -------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------- |
| `list_artifacts`     | `{}`                                                                                               | Titles, types, IDs, versions, descriptions, tags, pins, open comment count |
| `search_artifacts`   | `query`, optional `sort` (`relevance`/`updated`/`created`/`title`), `limit`                        | Ranked matches with an excerpt each, and `did_you_mean` for typos    |
| `get_artifact`       | `id`, optional `version` or `history:true`                                                         | Source of one revision (latest by default) + `runtime_errors`        |
| `create_artifact`    | `title`, `kind`, `content`, optional `files`, `description`, `icon`, `tags`, `note`                | `id`, `version`, `title`, `kind`, `warnings` (no source echo)        |
| `update_artifact`    | `id`, `base_version`, and `edits` or `content`; optional `files`, `title`, `kind`, `description`, `icon`, `tags`, `note` | `id`, `version`, `title`, `kind`, `warnings` (no source echo) |
| `artifact_storage`   | `id`, `op` (`get`/`set`/`delete`/`list`), `key`, `value`, `prefix`, `shared`                       | The artifact's `window.storage` data                                 |
| `artifact_comments`  | `id`, `op` (`list`/`add`/`resolve`/`reopen`), `body`, `parent`, `comment_id`, `all`                | Comment threads with replies (`to_claude` marks ones sent to you)    |
| `artifact_db`        | `id`, `op` (`get`/`list`/`query`/`set`/`update`/`str_replace`/`delete`/`batch`) and its fields      | The artifact's `claude.use("db")` documents (as Claude's ArtifactData) |
| `watch_artifacts`    | optional `ids`, `after` (cursor), `kinds`, `wait_seconds` (≤ 50)                                   | New versions, comments, data and asset changes since the cursor      |
| `delete_artifact`    | `id`, `base_version`, then `confirm`                                                               | First call: a confirmation token to show the person; second: deletes |
| `get_design_system`  | `{}`                                                                                               | Plain text: design rules + HTML starter (embeds CSS)                 |
| `get_app`            | optional `id`, `guide`, `version`, `files` (paths to read), `list_files`, `logs`, `tail`             | No `id`: apps + runtime. `guide:true`: the apps build contract with this server's facts. With `id`: versions, URLs, env names, links, and optionally files, contents and logs |
| `deploy_app`         | No `id`: `name`, `files` and/or `template:"notes"`, `description`, `icon`. With `id`: `base_version`, `files`/`binary_files` or `merge` with `edits`/`remove`; `note`, `promote` | Creates the app with its first version, or ships a new one: status, error and build log on failure, URLs |
| `manage_app`         | `id`, `op`: `promote` (`version`), `update` (`name`, `description`, `icon`, `pinned`, `env`), `restart`, `call` (`method`, `path`, `body`, `headers`), `share` (`version`, `read_only`), `unshare` (`token`), `delete` (then `confirm`) | Rollback, metadata and secrets, restarts, a backend request's status/headers/body, public links, two-step deletion |
| `list_media`         | `{}`                                                                                               | Media URLs, names, MIME types, sizes + upload URL and curl           |
| `begin_media_upload` | `mime_type`                                                                                        | Upload ID, starting offset, chunk limit                              |
| `upload_media_chunk` | `upload_id`, `offset`, `data_base64`, `final`                                                      | Next offset, or committed media URL                                  |

## How agents work with artifacts

**Edits.** For small changes an agent sends `edits`, a list of `{old_str, new_str}` pairs, instead of the whole source — the same shape as Claude's artifact `update` command. Each `old_str` must appear exactly once in the latest source (as changed by the edits before it); they apply in order, all or none, and a failure names the edit and why. Omitting `title`, `kind`, `description` or `icon` keeps them. Omitting both `content` and `edits` keeps the source, so a rename is one small call. An edit with `file` changes that supporting file instead of the main source.

**Files.** Like Claude's artifacts, an artifact can have supporting text files — stylesheets, JS modules, React components, JSON data, SVGs, more HTML pages — by relative path (`css/site.css`, `components/Chart.tsx`). The source references them relatively: `<link href="css/site.css">`, `<script type="module" src="js/app.js">`, `fetch("data.json")` (relative to the page), `<a href="about.html">` (other pages open in the same frame), or in React `import Chart from './components/Chart'` (`.tsx`, `.ts`, `.jsx`, `.js`, `.json` and `.css` imports are bundled; CSS is applied). `update_artifact`'s `files` adds or replaces files, `null` removes one, and files it does not name are kept. Up to 100 files of 2 MiB, 8 MiB per version with the source; binary assets such as photos belong in the Media library.

**Checks.** Every save returns `warnings` for problems the agent cannot see without a browser: React that does not compile (for example an unavailable package) and external URLs the sandbox will block. When someone opens a saved version, uncaught errors, failed loads and blocked requests from their browser are reported back and listed under `runtime_errors` in `get_artifact` (in memory, latest 20 per artifact). Draft previews are not reported.

Example agent prompt:

> Use shartifacts to create an interactive HTML packing checklist. Make it self-contained with inline styles and JavaScript, and give me its link. When I request changes, read the latest source and save a new version of the same artifact.

Every update requires the latest `base_version` from `get_artifact`. A stale save fails instead of overwriting someone else's changes. The browser refreshes agent updates within about three seconds; an unsaved browser draft stays intact and displays a conflict notice.

**Comments.** Leave notes on an artifact below its preview; select source text first to quote it. Agents see the open-thread count in `list_artifacts`, read threads with `artifact_comments`, reply, and resolve them once addressed. A typical loop: browse on your phone, comment, then ask your agent to "address the comments on the packing checklist".

Media uploads for agents (the curl upload, and base64 chunks for clients without a shell) are described in [media.md](media.md). Apps have their own three tools, described in [apps.md](apps.md).
