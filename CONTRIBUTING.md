# Contributing

shartifacts is an opinionated project, built and maintained for one person's use case. Bug reports and small fixes are welcome. For anything bigger, or anything that changes how it works for the sake of a different setup, forking it (or giving it to a coding agent to adapt) is usually the better route, and I may decline changes that don't fit how I use it.

That said, shartifacts aims to stay small. It should run once, at home or on a VPS, with as little to configure as possible, and with a security model a person can hold in their head. Changes that keep it that way are the easiest to accept.

## Before you start

- **Bugs**: open an issue with your mode (private, public sharing, edge), your platform, and the steps to reproduce. `docker compose logs shartifacts` helps.
- **Features**: open an issue first for anything larger than a small fix, so we can agree on the shape before you write it.
- **Security issues**: never in public. See [SECURITY.md](SECURITY.md).

## Development

You need Go 1.26 or newer. Node and Chrome are needed only for the browser tests and for rebuilding the vendored libraries.

```sh
go run . -data /tmp/shartifacts-dev     # http://localhost:8080
make check                              # vet, test, build
make help                               # other targets
```

[docs/development.md](docs/development.md) describes the code layout and the full test suites. AI coding agents should read [AGENTS.md](AGENTS.md), which has the same conventions in brief.

## Pull requests

- Keep each one to one change, with a test that fails without it.
- `gofmt`, `go vet` and `go test -race ./...` must pass. CI runs them, along with a Docker build.
- Update the docs page that describes what you changed. If setup or the modes change, update the setup skill in `skills/shartifacts-setup/` too.
- Use a Conventional Commit title: `feat(apps): …`, `fix(share): …`, `docs: …`.
- Do not include personal hostnames, IPs or paths in code, tests or docs.

By contributing, you agree that your contributions are licensed under the [MIT License](LICENSE).
