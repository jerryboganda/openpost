#!/usr/bin/env bash
# Deploy a tested OpenPost image to this VPS by immutable digest.
#
# Runs on the VPS only: pull, compose recreate, readiness/revision proof, and
# rollback. Building and testing happen on GitHub Actions. The script never
# prints environment values, database credentials, or registry secrets.
set -euo pipefail

DEPLOY_DIR=${OPENPOST_DEPLOY_DIR:-/opt/docker/openpost}
CONTAINER=${OPENPOST_CONTAINER:-openpost}
COMPOSE_FILE=${OPENPOST_COMPOSE_FILE:-docker-compose.yml}
OVERRIDE_FILE=${OPENPOST_OVERRIDE_FILE:-docker-compose.deploy.yml}
BACKUP_ROOT=${OPENPOST_BACKUP_ROOT:-$DEPLOY_DIR/backups}
READY_TIMEOUT=${OPENPOST_READY_TIMEOUT:-150}
BACKUP_KEEP=${OPENPOST_BACKUP_KEEP:-10}
DB_CONTAINER=${OPENPOST_DB_CONTAINER:-platform-postgres}
DB_USER=${OPENPOST_DB_USER:-openpost}
DB_NAME=${OPENPOST_DB_NAME:-openpost}
PUBLIC_URL=${OPENPOST_PUBLIC_URL:-}

image=""
revision=""
version=""
backup=1

usage() {
  cat <<'USAGE'
Usage: deploy.sh --image <registry/repo@sha256:...> --revision <full-git-sha> [options]

Options:
  --version <tag>    Version label expected from /api/v1/version
  --no-backup        Skip the pre-deploy Postgres backup (not recommended)
  --public-url <url> Also verify <url>/api/v1/ready and /version
  -h, --help         Show this help
USAGE
}

while [ $# -gt 0 ]; do
  case "$1" in
    --image) image=${2:?}; shift 2 ;;
    --revision) revision=${2:?}; shift 2 ;;
    --version) version=${2:?}; shift 2 ;;
    --no-backup) backup=0; shift ;;
    --public-url) PUBLIC_URL=${2:?}; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

fail() {
  echo "deploy: $*" >&2
  exit 1
}

[[ -n "$image" && -n "$revision" ]] || { usage >&2; fail "image and revision are required"; }
[[ "$image" == *"@sha256:"* ]] || fail "image must be pinned by digest (got: $image)"
[[ "$revision" =~ ^[0-9a-f]{40}$ ]] || fail "revision must be a full 40-character git sha"
[ -d "$DEPLOY_DIR" ] || fail "deploy directory not found: $DEPLOY_DIR"
cd "$DEPLOY_DIR"
[ -f "$COMPOSE_FILE" ] || fail "compose file not found: $COMPOSE_FILE"

mkdir -p "$BACKUP_ROOT"
chmod 700 "$BACKUP_ROOT"
exec 9>"$DEPLOY_DIR/.deploy.lock"
flock -n 9 || fail "another deployment is already running"

timestamp=$(date -u +%Y%m%dT%H%M%SZ)
work_dir="$BACKUP_ROOT/$timestamp"
mkdir -p "$work_dir"
chmod 700 "$work_dir"

previous_ref=$(docker inspect --format '{{.Config.Image}}' "$CONTAINER" 2>/dev/null || true)
previous_id=$(docker inspect --format '{{.Image}}' "$CONTAINER" 2>/dev/null || true)
previous_revision=""
if docker exec "$CONTAINER" wget -qO- --timeout=5 http://localhost:8080/api/v1/version >"$work_dir/previous-version.json" 2>/dev/null; then
  previous_revision=$(sed -n 's/.*"revision":"\([^"]*\)".*/\1/p' "$work_dir/previous-version.json" | head -1)
fi

rollback_tag=""
if [ -n "$previous_id" ]; then
  rollback_tag="openpost:rollback-$timestamp"
  docker tag "$previous_id" "$rollback_tag"
fi

write_override() {
  local ref=$1
  {
    printf 'services:\n  openpost:\n    image: %s\n' "$ref"
  } >"$OVERRIDE_FILE.tmp"
  mv "$OVERRIDE_FILE.tmp" "$OVERRIDE_FILE"
}

compose_up() {
  docker compose -f "$COMPOSE_FILE" -f "$OVERRIDE_FILE" up -d --no-deps "$CONTAINER" >/dev/null
}

container_ready() {
  docker exec "$CONTAINER" wget -qO- --timeout=5 http://localhost:8080/api/v1/ready 2>/dev/null |
    grep -q '"status":"ready"'
}

wait_ready() {
  local deadline=$((SECONDS + READY_TIMEOUT))
  while [ $SECONDS -lt $deadline ]; do
    if container_ready; then return 0; fi
    sleep 3
  done
  return 1
}

read_revision() {
  docker exec "$CONTAINER" wget -qO- --timeout=5 http://localhost:8080/api/v1/version 2>/dev/null |
    sed -n 's/.*"revision":"\([^"]*\)".*/\1/p' | head -1
}

write_result() {
  local status=$1
  local detail=$2
  local running=$3
  local backup_path=$4
  local file="$work_dir/result.json"
  {
    printf '{'
    printf '"status":"%s",' "$status"
    printf '"detail":"%s",' "$detail"
    printf '"image":"%s",' "$image"
    printf '"revision":"%s",' "$revision"
    printf '"version":"%s",' "$version"
    printf '"running_revision":"%s",' "$running"
    printf '"previous_image":"%s",' "$previous_ref"
    printf '"previous_revision":"%s",' "$previous_revision"
    printf '"rollback_image":"%s",' "$rollback_tag"
    printf '"backup":"%s",' "$backup_path"
    printf '"at":"%s"' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf '}\n'
  } >"$file"
  cp "$file" "$DEPLOY_DIR/last-deploy.json"
  cat "$file"
}

rollback() {
  local reason=$1
  local backup_path=$2
  echo "deploy: verification failed ($reason); rolling back to ${rollback_tag:-$previous_ref}" >&2
  if [ -z "$rollback_tag" ] && [ -z "$previous_ref" ]; then
    write_result failed "$reason" "" "$backup_path"
    fail "no previous image available for rollback"
  fi
  write_override "${rollback_tag:-$previous_ref}"
  if ! compose_up; then
    write_result failed "$reason; rollback recreate failed" "" "$backup_path"
    fail "rollback recreate failed"
  fi
  if ! wait_ready; then
    write_result failed "$reason; rollback not ready" "" "$backup_path"
    fail "rollback did not become ready"
  fi
  local running
  running=$(read_revision)
  write_result rolled_back "$reason" "$running" "$backup_path"
  fail "deployment failed verification ($reason); rolled back to $running"
}

backup_path=""
if [ "$backup" -eq 1 ]; then
  backup_path="$work_dir/postgres.dump"
  echo "deploy: backing up postgres database to $backup_path"
  if ! docker exec "$DB_CONTAINER" pg_dump -U "$DB_USER" -d "$DB_NAME" \
    --format=custom --no-owner --no-acl >"$backup_path"; then
    write_result failed "database backup failed" "" ""
    fail "database backup failed; refusing to deploy"
  fi
  [ -s "$backup_path" ] || { write_result failed "database backup is empty" "" "$backup_path"; fail "database backup is empty"; }
  if ! docker exec -i "$DB_CONTAINER" pg_restore -l <"$backup_path" >/dev/null 2>&1; then
    write_result failed "database backup is not a readable archive" "" "$backup_path"
    fail "database backup failed validation"
  fi
  chmod 600 "$backup_path"
fi

echo "deploy: pulling $image"
if ! docker pull "$image" >/dev/null; then
  write_result failed "image pull failed" "" "$backup_path"
  fail "image pull failed"
fi

write_override "$image"
echo "deploy: recreating $CONTAINER at $revision"
if ! compose_up; then
  rollback "compose recreate failed" "$backup_path"
fi

if ! wait_ready; then
  rollback "readiness check failed" "$backup_path"
fi

running_revision=$(read_revision)
if [ "$running_revision" != "$revision" ]; then
  rollback "revision mismatch (running: ${running_revision:-unknown})" "$backup_path"
fi

if [ -n "$PUBLIC_URL" ]; then
  public_ready=""
  for _ in $(seq 1 20); do
    public_ready=$(curl -fsS --max-time 5 "$PUBLIC_URL/api/v1/ready" 2>/dev/null || true)
    printf '%s' "$public_ready" | grep -q '"status":"ready"' && break
    sleep 3
  done
  if ! printf '%s' "$public_ready" | grep -q '"status":"ready"'; then
    rollback "public readiness failed" "$backup_path"
  fi
  public_version=$(curl -fsS --max-time 5 "$PUBLIC_URL/api/v1/version" 2>/dev/null || true)
  if ! printf '%s' "$public_version" | grep -q "\"revision\":\"$revision\""; then
    rollback "public revision mismatch" "$backup_path"
  fi
fi

write_result deployed "ready" "$running_revision" "$backup_path"

# Keep the newest backups; never delete the one just written.
find "$BACKUP_ROOT" -mindepth 1 -maxdepth 1 -type d -name '20*' -printf '%f\n' |
  sort -r | tail -n +$((BACKUP_KEEP + 1)) | while read -r old; do
    [ "$old" = "$timestamp" ] && continue
    rm -rf "$BACKUP_ROOT/$old"
  done

echo "deploy: verified $running_revision with image $image"
