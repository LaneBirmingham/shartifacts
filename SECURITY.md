# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately, through **GitHub's private vulnerability reporting** (the *Security* tab, then *Report a vulnerability*), rather than in a public issue. Include the version or commit, your configuration (mode, edge, any proxy), the steps to reproduce, and what an attacker gains.

You should get a reply within a week. Fixes are released as soon as they are ready, and credited unless you prefer otherwise.

## What counts

shartifacts has no login by design. Anyone who can reach the workspace can do everything, so "someone on my LAN can delete artifacts" is expected behaviour, not a vulnerability. These are vulnerabilities, for example:

- Reaching the workspace, `/api` or `/mcp` from a public address, or through the share hostname, an app hostname or the public listener (port 8081)
- Reading or changing anything with a share token beyond what that token grants, or writing through a read-only link
- An artifact or app page escaping its sandbox to read the workspace or another artifact's data
- An app backend escaping the runner sidecar or its Landlock confinement on a supported setup
- DNS rebinding or cross-site request paths into the workspace

[docs/security.md](docs/security.md) describes the full model.

## Supported versions

Only the latest release, and `main`, get security fixes. Upgrade with `git pull && docker compose up -d --build`.
