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
- **Settings** are flags in `internal/config`, each with a `SHARTIFACTS_*` variable. A new one also goes in `compose.yaml` (when Compose users need it), `.env.example` and `docs/configuration.md`.
- **Docs move with behaviour.** Update the page in `docs/` that describes what you changed, and the setup skill if setup or the modes change.
- **No personal deployment details** (hostnames, IPs, paths, names) in code, tests or docs: use `example.com`, `192.168.1.20` and `myserver.tailnet-name.ts.net`.
- Commits follow Conventional Commits (`feat(apps): …`, `fix(share): …`, `docs: …`).
