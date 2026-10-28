#!/bin/sh
# Ship a checkout to the server from a CI job. Run by .gitea/workflows/ci.yml on `main`.
#
# A job's filesystem is not visible to the docker daemon, and a directory the runner happened
# to mount is not something a script should assume, so the tree is piped into a container that
# asks for exactly the two mounts it needs. Both are host paths, because compose resolves bind
# mounts on the daemon: the deploy directory is mounted at its own absolute path, so
# `compose.yaml`'s relative paths mean the same thing inside the container and on the host.
#
# Everything else — the .env guard, the rebuild, the healthchecks, the commit record — is
# scripts/deploy-tower.sh, the same script a person runs by hand.
set -eu

DEPLOY_DIR=${DEPLOY_DIR:-/mnt/user/projects/shartifacts}
IMAGE=${IMAGE:-shartifacts-ci:local}

[ -f go.mod ] || { echo "refusing: run this from the root of the repository" >&2; exit 1; }

SHA=$(git rev-parse --short HEAD)
REV=$(git rev-parse HEAD)
SUBJECT=$(git log -1 --pretty=%s)

tar -cf - --exclude=.git --exclude=node_modules --exclude=bin --exclude=data . | docker run --rm -i \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$DEPLOY_DIR:$DEPLOY_DIR" \
  -e DEPLOY_DIR="$DEPLOY_DIR" -e SRC=/src \
  -e SHA="$SHA" -e REV="$REV" -e SUBJECT="$SUBJECT" \
  "$IMAGE" sh -c 'mkdir -p /src && tar -xf - -C /src && sh /src/scripts/deploy-tower.sh'
