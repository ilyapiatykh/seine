#!/bin/sh
# Entrypoint for the demo git-server container.
#
# Initialises /srv/repos/seine.git as a bare repository populated with
# the seed network.yaml on first boot, then serves it over the native
# git daemon protocol (port 9418). receive-pack is enabled so an
# operator can `docker compose exec gitserver git push` updates from
# inside the container during the demo.

set -eu

REPO_ROOT="/srv/repos"
REPO_DIR="${REPO_ROOT}/seine.git"
SEED_FILE="/seed/network.yaml"

mkdir -p "${REPO_ROOT}"

if [ ! -d "${REPO_DIR}" ]; then
  if [ ! -f "${SEED_FILE}" ]; then
    echo "gitserver: seed file ${SEED_FILE} missing; mount it via docker compose volume" >&2
    exit 1
  fi

  git init --bare --initial-branch=main "${REPO_DIR}"

  # Allow `git push` over the daemon protocol so the demo can introduce
  # spec changes without rebuilding the image.
  git -C "${REPO_DIR}" config daemon.receivepack true

  WORK="$(mktemp -d)"
  trap 'rm -rf "${WORK}"' EXIT
  git -c init.defaultBranch=main clone "${REPO_DIR}" "${WORK}/clone"
  cp "${SEED_FILE}" "${WORK}/clone/network.yaml"
  cd "${WORK}/clone"
  git -c user.email=demo@seine.local -c user.name=demo add network.yaml
  git -c user.email=demo@seine.local -c user.name=demo commit -m "initial spec"
  git push origin main
  cd /
fi

echo "gitserver: serving ${REPO_DIR} on tcp/9418"
exec git daemon \
  --reuseaddr \
  --listen=0.0.0.0 \
  --port=9418 \
  --base-path="${REPO_ROOT}" \
  --export-all \
  --enable=receive-pack \
  --verbose
