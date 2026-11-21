# Instructions for AI agents

## Setting shartifacts up for someone?

Follow [skills/shartifacts-setup/SKILL.md](skills/shartifacts-setup/SKILL.md). It covers the questions to ask, the plan, installing, verifying and connecting. Its first rule overrides anything else: **never expose the workspace (port 8080) to the internet.**

## Using shartifacts?

Connect to its MCP endpoint (`/mcp`). The server sends instructions for its tools when you connect. [docs/agents.md](docs/agents.md) describes them.

## Working on the code

shartifacts is a Go server (`main.go`, `internal/`) with a plain-JavaScript UI (`web/`) embedded in the binary. [docs/development.md](docs/development.md) has the layout and every test command.

Before you finish a change:

```sh
gofmt -l .            # prints nothing
go vet ./...
go test ./...         # add -race when you touch concurrency
```

Run `npm run test:browser` when you change `web/`, and `npm run test:apps` when you change apps or the runner (they need Chrome or Playwright Chromium). For Compose, Dockerfile or `deploy/` changes, run `docker compose config -q`, and bring the stack up if you can.

Conventions:

- **Security first.** `internal/server/server.go` (`guard`) and `forwarded.go` decide who reaches anything, and `auth.go` (with `internal/auth`) who is signed in; `share.go` and `apps_serve.go` decide what the public hostnames serve. Changes there need a test in the matching `*_test.go` that shows both what is allowed and what is refused. Never trust forwarded headers to *grant* access.
- **Comments say why**, in full sentences, as the surrounding code does. Match the existing style rather than adding a framework or a new dependency. The standard library comes first.
- **Keep the MCP tool count small.** Every tool costs each connected agent context, and a short list is easier to choose from. Give new abilities to an existing tool as a field or an `op`, the way `manage_app` (`op`), `artifact_db` (`op`, `batch`) and `list_artifacts` (`archived`) do. Say what the field does in its `jsonschema` text and the tool's description, so agents find it without a new name. Add a tool only when no existing tool is a natural home, and say why in the change. The tool list in `scripts/browser-test.mjs` is the check.
- **Settings** are flags in `internal/config`, each with a `SHARTIFACTS_*` variable. A new one also goes in `compose.yaml` (when Compose users need it), `.env.example` and `docs/configuration.md`.
- **Docs move with behaviour.** Update the page in `docs/` that describes what you changed, and the setup skill if setup or the modes change.
- **No personal deployment details** (hostnames, IPs, paths, names) in code, tests or docs: use `example.com`, `192.168.1.20` and `myserver.tailnet-name.ts.net`.
- Commits follow Conventional Commits (`feat(apps): …`, `fix(share): …`, `docs: …`).

## Progress notes for long-running work

Long tasks outlive the context window. Compaction keeps a short summary and drops the rest, so keep the working state on disk where a compacted or brand-new session can pick it up.

- **Start with a progress folder.** When you begin a goal that will take more than a few steps, create `progress/` at the project root (it is git-ignored) and a file for the goal, e.g. `progress/<task-slug>.md`. One file per goal.
- **What goes in it:** the goal and what "done" means; a checklist of steps (done, in progress, next); decisions and why they were made; approaches that failed and why; key files, commands and URLs; open questions for Lane.
- **Keep it current.** Update it at every milestone and before every commit, so it is never far behind when auto-compaction happens. Before a manual compact, update it first. Write it so a session with no history could carry on from it alone.
- **Read it first.** After a compaction, or when starting or resuming work, read `progress/` before re-investigating anything.
- **When compacting**, keep the current goal, the open checklist items and the path to the progress file in the summary.
- **Clean up as you go.** Prune finished or stale entries at milestones so the file stays short. When the goal is done, or before the final commit, move anything worth keeping (design decisions, setup or run notes, gotchas) into the README or `docs/`, then delete the goal's progress file. Never reference `progress/` from committed code or docs.
