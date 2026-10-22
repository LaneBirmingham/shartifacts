# HTTP API

The UI and MCP share the same store. JSON requests must use `Content-Type: application/json`.

```sh
curl -H 'Content-Type: application/json' http://localhost:8080/api/artifacts \
  -d '{"title":"Hello","kind":"html","content":"<h1>Hello, local network.</h1>"}'

curl http://localhost:8080/api/artifacts
curl http://localhost:8080/api/artifacts/ARTIFACT_ID

curl -X PUT -H 'Content-Type: application/json' \
  http://localhost:8080/api/artifacts/ARTIFACT_ID \
  -d '{"title":"Hello again","kind":"html","content":"<h1>Version two</h1>","base_version":1}'

# Small change: edits instead of content; omitted title/kind/description are kept
curl -X PUT -H 'Content-Type: application/json' \
  http://localhost:8080/api/artifacts/ARTIFACT_ID \
  -d '{"edits":[{"old_str":"Version two","new_str":"Version three"}],"base_version":2}'

curl -OJ 'http://localhost:8080/api/artifacts/ARTIFACT_ID/download?version=1'
curl -X DELETE 'http://localhost:8080/api/artifacts/ARTIFACT_ID?base_version=3'
```

| Endpoint | Body | Purpose |
| --- | --- | --- |
| `POST /api/artifacts/{id}/storage` | `{op, key, value, shared, prefix}` | `window.storage` operations; results match Claude's (`get` of a missing key returns `null`) |
| `GET/POST /api/artifacts/{id}/comments` | `{body, parent, quote, author}` | List or add comments |
| `PUT /api/artifacts/{id}/comments/{comment}` | `{resolved}` | Resolve or reopen a thread |
| `PUT /api/artifacts/{id}/pin` | `{pinned}` | Pin to the top of the library (no new revision) |
| `GET /api/search?q=&kind=&tag=&has=&updated=&sort=&limit=` | | Ranked search with excerpts (`q` takes the filters in **Finding things**; the other parameters add them, and `tag` repeats) |
| `GET /api/artifacts/{id}/related` | | Artifacts sharing its vocabulary, and suggested tags |
| `POST /api/artifacts/{id}/errors` | `{version, message}` | Record a runtime error seen in a saved version |
| `POST /api/complete` | `{prompt}` or `{messages}` | `window.claude.complete`; `503` unless `ANTHROPIC_API_KEY` is set |
| `POST /api/media?name=FILE` | raw bytes | Upload media or a data file, keeping its name |
| `GET/POST /api/people` | `{name}` | People; requests act as the one named in `X-Shartifacts-Viewer` |
| `POST /api/artifacts/{id}/db` | `{op, path, data, query, holder, ttlMs}` | `db` operations (`get`, `query`, `set`, `update`, `delete`, `acquire`) |
| `GET /api/artifacts/{id}/live?peer=&viewer=` | | Server-sent events: data, version, comment and room events |
| `POST /api/artifacts/{id}/room` | `{peer, room, op, topic, data, patch}` | `room` joins, presence and messages |
| `POST/GET /api/artifacts/{id}/assets`, `DELETE …/assets/{asset}`, `GET /_blob/{asset}` | raw bytes | Artifact assets |
| `POST /api/artifacts/{id}/publish` | `{base_version, html}` or `{base_version, files}` | The `artifact` capability's compare-and-set publish |
| `POST /api/sample`, `GET /api/sample/limits` | Messages-API turns and tools | The `sample` capability, one round at a time |
| `GET /api/connectors`, `POST /api/connectors/call`, `POST /api/connectors/describe` | `{server, tool, input}` | The `mcp` capability's connectors |
| `GET/POST /api/artifacts/{id}/shares`, `DELETE /api/shares/{token}`, `GET /api/config` | `{version}` (0 follows the latest) | Read-only share links; `share_origin` says whether sharing is on |
| `GET /s/{token}` (and `/meta`, `/preview…`, `/api/me`, `POST /api/db` and `/api/storage` reads, `/api/live`) | | The share page and its read-only runtime, on the share origin |
| `GET/POST /api/apps` | `{name, description, icon, files, binary_files, template}` or a zip/tar.gz body with `?name=` | List apps (with the runtime) or create one with its first version |
| `GET/PATCH/DELETE /api/apps/{id}` | `{name, description, icon, pinned}` | Read (`?logs` adds build logs), change or delete an app |
| `POST /api/apps/{id}/deployments` | `{base_version, files, binary_files, merge, remove, edits, note, promote}` or a zip/tar.gz body with `?note=&merge=&promote=&base_version=` | Deploy a version (`201`; `202` while it is still building) |
| `GET /api/apps/{id}/deployments/{n}/files[/{path}]`, `GET /api/apps/{id}/download?version=` | | A version's file list, one file, or its source as a zip |
| `PUT /api/apps/{id}/live` | `{version}` | Promote or roll back |
| `GET /api/apps/{id}/logs?version=&tail=`, `POST /api/apps/{id}/restart` | | Logs; stop the backends (the next request starts them) |
| `PUT /api/apps/{id}/env` | `{vars: {NAME: "value" or null}}` | Backend variables and secrets |
| `POST /api/apps/{id}/shares`, `DELETE /api/app-shares/{token}` | `{version, read_only}` (0 follows live) | Public app links |
| `GET /api/apps/guide` | | The build contract as text |
| `/run/{id}/{live or n}/…`, `/p/{token}/…` | | An app version's pages, and its backend under `api/` |

`POST` and `PUT /api/artifacts` accept `files` (`{"path": "content"}`, with `null` removing a file on update) and edits with `file`. `GET /preview/{id}?version=N` renders a saved version in a sandbox; a version with files redirects to `/preview/{id}/{N}/`, which serves the page with its files beneath it. `GET /view/{id}` is the full-page view. `GET /healthz` is a process health check. Browser live previews use `POST /api/previews` with `title`, `kind`, `content` and optional `files`, returning a one-use `url` that expires after two minutes (with files, a directory URL readable until it expires). Preview drafts are ephemeral and do not enter artifact history.
