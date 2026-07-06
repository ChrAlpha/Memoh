#!/usr/bin/env bash
set -euo pipefail

ROOT=$(CDPATH= cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"

usage() {
  cat <<'EOF'
Usage: scripts/dev.sh <command> <stack> [args...]

Commands:
  up        Build and start the stack, then follow high-signal logs
  logs      Follow logs; defaults to server web
  status    Show compose status and service URLs
  down      Stop the stack
  restart   Restart one or more services
  build     Build the stack images

Stacks:
  postgres
  postgres-webhook
  postgres-minify
  postgres-minify-webhook
  postgres-selinux
  sqlite
  sqlite-webhook
  sqlite-minify
  sqlite-minify-webhook
EOF
}

die() {
  echo "dev: $*" >&2
  exit 1
}

sanitize_slug() {
  local raw="$1"
  local slug
  slug=$(printf '%s' "$raw" | tr '[:upper:]' '[:lower:]' | sed -E 's/[^a-z0-9]+/-/g; s/^-+//; s/-+$//')
  printf '%s' "${slug:-memoh}"
}

default_project() {
  local stack="$1"
  local checkout
  local base

  checkout=$(basename "$ROOT")
  if [[ "$checkout" == "Memoh" ]]; then
    base="memoh-dev"
  else
    base="memoh-dev-$(sanitize_slug "$checkout")"
  fi

  if [[ "$stack" == sqlite* ]]; then
    if [[ "$base" == "memoh-dev" ]]; then
      base="memoh-dev-sqlite"
    else
      base="$base-sqlite"
    fi
  fi

  printf '%s' "$base"
}

stack_files() {
  case "$1" in
    postgres) printf '%s\n' devenv/docker-compose.yml ;;
    postgres-webhook) printf '%s\n' devenv/docker-compose.yml devenv/docker-compose.webhook-tunnel.yml ;;
    postgres-minify) printf '%s\n' devenv/docker-compose.minify.yml ;;
    postgres-minify-webhook) printf '%s\n' devenv/docker-compose.minify.yml devenv/docker-compose.webhook-tunnel.yml ;;
    postgres-selinux) printf '%s\n' devenv/docker-compose.yml devenv/docker-compose.selinux.yml ;;
    sqlite) printf '%s\n' devenv/docker-compose.sqlite.yml ;;
    sqlite-webhook) printf '%s\n' devenv/docker-compose.sqlite.yml devenv/docker-compose.webhook-tunnel.yml ;;
    sqlite-minify) printf '%s\n' devenv/docker-compose.sqlite.minify.yml ;;
    sqlite-minify-webhook) printf '%s\n' devenv/docker-compose.sqlite.minify.yml devenv/docker-compose.webhook-tunnel.yml ;;
    *) die "unknown stack '$1'" ;;
  esac
}

stack_web_url() {
  case "$1" in
    sqlite*) printf 'http://localhost:%s' "${MEMOH_SQLITE_DEV_WEB_PORT:-19082}" ;;
    *) printf 'http://localhost:%s' "${MEMOH_DEV_WEB_PORT:-18082}" ;;
  esac
}

stack_api_url() {
  case "$1" in
    sqlite*) printf 'http://localhost:%s' "${MEMOH_SQLITE_DEV_SERVER_PORT:-19080}" ;;
    *) printf 'http://localhost:%s' "${MEMOH_DEV_SERVER_PORT:-18080}" ;;
  esac
}

compose_args=()
files=()
project=""
stack=""

prepare_compose() {
  stack="$1"
  files=()
  local file
  while IFS= read -r file; do
    files+=("$file")
  done < <(stack_files "$stack")
  project="${MEMOH_DEV_PROJECT:-$(default_project "$stack")}"
  export MEMOH_DEV_PROJECT="$project"
  export MEMOH_DEV_CONTAINER_PREFIX="${MEMOH_DEV_CONTAINER_PREFIX:-$project}"
  export COMPOSE_PROJECT_NAME="$project"

  compose_args=(-p "$project")
  for file in "${files[@]}"; do
    compose_args+=(-f "$file")
  done
}

print_command() {
  printf 'docker compose'
  local arg
  for arg in "${compose_args[@]}" "$@"; do
    printf ' %s' "$arg"
  done
  printf '\n'
}

run_compose() {
  if [[ "${MEMOH_DEV_DRY_RUN:-}" == "1" ]]; then
    print_command "$@"
    return 0
  fi
  docker compose "${compose_args[@]}" "$@"
}

run_compose_logged() {
  local log_file="$1"
  shift

  if [[ "${MEMOH_DEV_DRY_RUN:-}" == "1" ]]; then
    print_command "$@"
    return 0
  fi

  {
    printf '$ docker compose'
    local arg
    for arg in "${compose_args[@]}" "$@"; do
      printf ' %q' "$arg"
    done
    printf '\n'
  } >> "$log_file"

  if [[ "${MEMOH_DEV_VERBOSE:-}" == "1" ]]; then
    docker compose "${compose_args[@]}" "$@" 2>&1 | tee -a "$log_file"
    return "${PIPESTATUS[0]}"
  fi

  if ! docker compose "${compose_args[@]}" "$@" >> "$log_file" 2>&1; then
    echo "dev: command failed; last lines from $log_file" >&2
    tail -n 80 "$log_file" >&2 || true
    return 1
  fi
}

print_summary() {
  local log_file="$1"

  cat <<EOF

Memoh dev
  stack:   $stack
  project: $project
  web:     $(stack_web_url "$stack")
  api:     $(stack_api_url "$stack")
  logs:    $log_file

Commands:
  mise run dev:status
  mise run dev:logs -- server web
  mise run dev:down
EOF
}

filter_default_logs() {
  awk '
    / uri=\/health / { next }
    /"HEAD \/health / { next }
    /"GET \/health / { next }
    { print; fflush() }
  '
}

follow_logs() {
  local services=("$@")
  if [[ "${#services[@]}" -eq 0 ]]; then
    services=(server web)
  elif [[ "${services[0]}" == "--all" ]]; then
    services=()
  fi

  if [[ "${MEMOH_DEV_DRY_RUN:-}" == "1" ]]; then
    run_compose logs -f --tail=120 "${services[@]}"
    return 0
  fi

  run_compose logs -f --tail=120 "${services[@]}" | filter_default_logs
}

warn_foreign_project() {
  if [[ "${MEMOH_DEV_DRY_RUN:-}" == "1" ]] || ! command -v docker >/dev/null 2>&1; then
    return 0
  fi

  local foreign
  foreign=$(docker ps -a \
    --filter "label=com.docker.compose.project=$project" \
    --format '{{.Names}}	{{.Label "com.docker.compose.project.working_dir"}}' \
    | awk -v root="$ROOT" '$2 != "" && index($2, root) != 1 { print }' || true)

  if [[ -n "$foreign" ]]; then
    echo "dev: warning: project '$project' also has containers from another checkout:" >&2
    printf '%s\n' "$foreign" >&2
    echo "dev: set MEMOH_DEV_PROJECT or run dev:down for the stale checkout if this is unintended." >&2
  fi
}

cmd_up() {
  local log_dir=".tmp/dev"
  local log_file="$log_dir/$stack-up.log"
  mkdir -p "$log_dir"
  : > "$log_file"

  warn_foreign_project

  echo "Starting Memoh dev stack ($stack)..."
  echo "  project: $project"
  echo "  details: $log_file"

  run_compose_logged "$log_file" build
  run_compose_logged "$log_file" up -d --remove-orphans
  print_summary "$log_file"

  if [[ "${MEMOH_DEV_ATTACH:-1}" != "0" ]]; then
    follow_logs server web
  fi
}

cmd_status() {
  print_summary ".tmp/dev/$stack-up.log"
  run_compose ps
}

command="${1:-}"
if [[ -z "$command" || "$command" == "-h" || "$command" == "--help" ]]; then
  usage
  exit 0
fi
shift || true

stack="${1:-postgres}"
shift || true
prepare_compose "$stack"

case "$command" in
  up) cmd_up "$@" ;;
  logs) follow_logs "$@" ;;
  status|ps) cmd_status ;;
  down) run_compose down --remove-orphans "$@" ;;
  restart) [[ "$#" -gt 0 ]] || die "restart requires at least one service"; run_compose restart "$@" ;;
  build) mkdir -p .tmp/dev; run_compose_logged ".tmp/dev/$stack-build.log" build "$@" ;;
  *) die "unknown command '$command'" ;;
esac
