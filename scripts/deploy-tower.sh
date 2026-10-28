#!/bin/sh
# Deploy a checkout to the server's compose stack.
#
# Run by CI on `main`, and by hand when something needs shipping outside CI:
#   SRC=~/Dev/shartifacts sh scripts/deploy-tower.sh
#
# What it guarantees:
#   - .env is never written. It is the server's, not the repository's; the script copies it,
#     compares it after the copy, and stops if it changed.
#   - Nothing is deleted, so a stray file cannot take the data directory with it.
#   - The built images are tagged with the commit, and the previous ones are kept as
#     `:rollback`, so a bad deploy is undone without a six-minute rebuild:
#         docker tag shartifacts:rollback shartifacts:local
#         docker tag shartifacts-runner:rollback shartifacts-runner:local
#         docker compose up -d --no-build
#   - The commit that is running is recorded in .deployed.
set -eu

DEPLOY_DIR=${DEPLOY_DIR:-/mnt/user/projects/shartifacts}
SRC=${SRC:-$(pwd)}
PROJECT=${PROJECT:-shartifacts}
OWNER=${OWNER:-1001:100}
KEEP_IMAGES=${KEEP_IMAGES:-5}

[ -f "$SRC/go.mod" ] || { echo "refusing: SRC=$SRC does not look like the repository" >&2; exit 1; }
[ -f "$DEPLOY_DIR/.env" ] || { echo "refusing: $DEPLOY_DIR/.env is missing" >&2; exit 1; }

SHA=$(git -C "$SRC" rev-parse --short HEAD)
REV=$(git -C "$SRC" rev-parse HEAD)
SUBJECT=$(git -C "$SRC" log -1 --pretty=%s)
echo "deploying $SHA — $SUBJECT"

ENV_BEFORE=$(sha256sum "$DEPLOY_DIR/.env" | cut -d' ' -f1)

# No --delete: the destination holds .env, and a mistyped path must not be able to empty
# anything. The excluded paths are the ones that are not source: node_modules and vendor
# output are built inside the image, data and bin belong to the running stack.
rsync -a \
  --exclude='.env' --exclude='.env.bak*' \
  --exclude='.git/' --exclude='node_modules/' \
  --exclude='data/' --exclude='bin/' --exclude='test-results/' \
  --exclude='progress/' \
  "$SRC/" "$DEPLOY_DIR/"

ENV_AFTER=$(sha256sum "$DEPLOY_DIR/.env" | cut -d' ' -f1)
[ "$ENV_BEFORE" = "$ENV_AFTER" ] || { echo "refusing: .env changed during the copy" >&2; exit 1; }

# Keep the running images as the rollback target before they are replaced.
docker image tag "${PROJECT}:local" "${PROJECT}:rollback" 2>/dev/null || true
docker image tag "${PROJECT}-runner:local" "${PROJECT}-runner:rollback" 2>/dev/null || true

cd "$DEPLOY_DIR"
docker compose up -d --build

docker image tag "${PROJECT}:local" "${PROJECT}:ci-${SHA}"
docker image tag "${PROJECT}-runner:local" "${PROJECT}-runner:ci-${SHA}"

healthy() {
  [ "$(docker inspect -f '{{.State.Health.Status}}' "$1" 2>/dev/null)" = "healthy" ]
}
i=0
while [ "$i" -lt 60 ]; do
  if healthy "$PROJECT" && healthy "$PROJECT-runner"; then
    break
  fi
  i=$((i + 1))
  [ "$i" -ge 60 ] && { echo "not healthy after 5 minutes" >&2; docker compose logs --tail 40 >&2; exit 1; }
  sleep 5
done

printf '%s\n' "$REV" > "$DEPLOY_DIR/.deployed"

# Every file the copy touched goes back to the user the rest of the directory belongs to,
# so a later rsync from a workstation does not fight root-owned files. .env keeps its own
# owner: it is the server's file and nothing here should reason about it.
find "$DEPLOY_DIR" \( -name '.env' -o -name '.env.bak*' -o -name '.deployed' \) -prune -o \
  -exec chown "$OWNER" {} + 2>/dev/null || true

# Old deploy tags pile up one pair per push; keep the newest few for rollback.
docker images --format '{{.Repository}}:{{.Tag}} {{.CreatedAt}}' \
  | grep -E "^${PROJECT}(-runner)?:ci-" | sort -k2 -r | tail -n +$((KEEP_IMAGES * 2 + 1)) \
  | awk '{print $1}' | xargs -r docker image rm >/dev/null 2>&1 || true

echo "deployed $SHA; $(docker inspect -f '{{.State.Health.Status}}' "$PROJECT")"
