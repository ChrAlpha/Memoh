#!/bin/sh
set -eu

ROOT=$(CDPATH= cd "$(dirname "$0")/.." && pwd)

OUTPUT=$(MEMOH_DEV_DRY_RUN=1 MEMOH_DEV_PROJECT=memoh-dev-test bash "$ROOT/scripts/dev.sh" up postgres 2>&1)

printf '%s\n' "$OUTPUT" | grep -q "docker compose -p memoh-dev-test -f devenv/docker-compose.yml build" || {
  echo "expected quiet build command in dry-run output" >&2
  printf '%s\n' "$OUTPUT" >&2
  exit 1
}

printf '%s\n' "$OUTPUT" | grep -q "docker compose -p memoh-dev-test -f devenv/docker-compose.yml up -d --remove-orphans" || {
  echo "expected detached up command in dry-run output" >&2
  printf '%s\n' "$OUTPUT" >&2
  exit 1
}

printf '%s\n' "$OUTPUT" | grep -Eq "logs:[[:space:]]+\\.tmp/dev/postgres-up\\.log" || {
  echo "expected dev log artifact path in dry-run output" >&2
  printf '%s\n' "$OUTPUT" >&2
  exit 1
}

LOG_OUTPUT=$(MEMOH_DEV_DRY_RUN=1 MEMOH_DEV_PROJECT=memoh-dev-test bash "$ROOT/scripts/dev.sh" logs postgres 2>&1)

printf '%s\n' "$LOG_OUTPUT" | grep -q "docker compose -p memoh-dev-test -f devenv/docker-compose.yml logs -f --tail=120 server web" || {
  echo "expected default log follow to focus on server and web" >&2
  printf '%s\n' "$LOG_OUTPUT" >&2
  exit 1
}

grep -q "scripts/dev.sh up postgres" "$ROOT/mise.toml" || {
  echo "expected mise dev task to route through scripts/dev.sh" >&2
  exit 1
}

grep -q 'container_name: "${MEMOH_DEV_CONTAINER_PREFIX:-memoh-dev}-server"' "$ROOT/devenv/docker-compose.yml" || {
  echo "expected postgres compose stack to use the dev container prefix" >&2
  exit 1
}
