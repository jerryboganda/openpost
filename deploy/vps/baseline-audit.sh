#!/usr/bin/env bash
# Streamed over SSH; reads the known OpenPost VPS paths without writing to the host.
set -euo pipefail
export LC_ALL=C TZ=UTC

deploy_dir=/opt/docker/openpost
backup_dir=$deploy_dir/backups
container=openpost
max_entries=150

field() {
  local name=$1 value=${2:-} pattern=$3
  if [[ -z "$value" || "$value" == '<no value>' ]]; then
    printf '%s=missing\n' "$name"
  elif [[ "$value" =~ $pattern ]]; then
    printf '%s=%s\n' "$name" "$value"
  else
    printf '%s=unavailable_or_redacted\n' "$name"
  fi
}

json_string() {
  local document=$1 key=$2
  printf '%s\n' "$document" |
    sed -nE "s/.*\"$key\"[[:space:]]*:[[:space:]]*\"([^\"]*)\".*/\1/p" | head -n 1
}

safe_path() {
  local path=$1
  case "${path,,}" in
    *'.env'*|*'secret'*|*'credential'*|*'password'*|*'token'*|*'private'*|*'.pem'*|*'.key'*|*'.p12'*|*'.pfx'*|*'id_rsa'*|*'id_ed25519'*|*'known_hosts'*|*'authorized_keys'*|*'.ssh'*|*'.npmrc'*)
      printf '[redacted]'
      ;;
    *)
      if [[ ${#path} -le 180 && "$path" =~ ^[a-zA-Z0-9_./@+-]+$ ]]; then
        printf '%s' "$path"
      else
        printf '[redacted]'
      fi
      ;;
  esac
}

printf 'audit.scope=read_only_default_openpost_paths\n'
printf 'audit.collected_utc=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
printf 'source.expected_directory=%s\n' "$deploy_dir"

if [[ ! -d "$deploy_dir" || -L "$deploy_dir" ]]; then
  printf 'source.directory=missing_or_symlink\n'
elif [[ ! -e "$deploy_dir/.git" ]]; then
  printf 'source.checkout=missing_git_marker\n'
elif ! command -v git >/dev/null 2>&1; then
  printf 'source.checkout=git_unavailable\n'
else
  root=$(git -C "$deploy_dir" rev-parse --show-toplevel 2>/dev/null || true)
  physical_dir=$(cd "$deploy_dir" && pwd -P)
  if [[ "$root" != "$physical_dir" ]]; then
    printf 'source.checkout=unexpected_root_or_unreadable\n'
  else
    printf 'source.checkout=present\n'
    head=$(git -C "$deploy_dir" rev-parse --verify HEAD 2>/dev/null || true)
    field source.head "$head" '^[0-9a-f]{40}$'
    if status=$(git -C "$deploy_dir" -c core.quotePath=true status --porcelain=v1 --untracked-files=all 2>/dev/null); then
      total=0
      shown=0
      while IFS= read -r line; do
        [[ -n "$line" ]] || continue
        total=$((total + 1))
        if (( shown >= max_entries )); then continue; fi
        shown=$((shown + 1))
        path=$(safe_path "${line:3}")
        index_status=${line:0:1}
        worktree_status=${line:1:1}
        if [[ "${line:0:2}" == '??' ]]; then
          printf 'source.untracked=%s\n' "$path"
        else
          if [[ "$index_status" != ' ' ]]; then
            printf 'source.staged[%s]=%s\n' "$index_status" "$path"
          fi
          if [[ "$worktree_status" != ' ' ]]; then
            printf 'source.unstaged[%s]=%s\n' "$worktree_status" "$path"
          fi
        fi
      done <<< "$status"
      printf 'source.status_entries=%s\nsource.status_shown=%s\n' "$total" "$shown"
      if (( total > shown )); then printf 'source.status_truncated=true\n'; fi
      if (( total == 0 )); then printf 'source.clean=true\n'; else printf 'source.clean=false\n'; fi
    else
      printf 'source.status=unavailable\n'
    fi
  fi
fi

if ! command -v docker >/dev/null 2>&1; then
  printf 'docker=command_missing\n'
elif ! details=$(docker container inspect --format '{{.Name}}|{{.Id}}|{{.Config.Image}}|{{.Image}}|{{.State.Running}}' "$container" 2>/dev/null); then
  printf 'docker.container=missing_or_inaccessible\n'
else
  IFS='|' read -r name container_id image_ref image_id running <<< "$details"
  field docker.container_name "$name" '^/[a-zA-Z0-9_.-]{1,80}$'
  field docker.container_id "$container_id" '^[0-9a-f]{64}$'
  field docker.image_ref "$image_ref" '^[a-zA-Z0-9][a-zA-Z0-9._/:@+-]{0,240}$'
  field docker.image_id "$image_id" '^sha256:[0-9a-f]{64}$'
  field docker.running "$running" '^(true|false)$'

  if [[ "$image_id" =~ ^sha256:[0-9a-f]{64}$ ]] &&
     image_details=$(docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$image_id" 2>/dev/null); then
    digest_count=0
    while IFS= read -r digest; do
      if (( digest_count >= 20 )); then break; fi
      if [[ "$digest" =~ ^[a-zA-Z0-9][a-zA-Z0-9._/:-]{0,180}@sha256:[0-9a-f]{64}$ ]]; then
        printf 'docker.repo_digest=%s\n' "$digest"
        digest_count=$((digest_count + 1))
      fi
    done <<< "$image_details"
    if (( digest_count == 0 )); then printf 'docker.repo_digests=missing\n'; fi
    if (( digest_count == 20 )); then printf 'docker.repo_digests=limited_to_20\n'; fi
    field docker.oci_revision "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$image_id" 2>/dev/null || true)" '^[a-zA-Z0-9._+-]{1,100}$'
    field docker.oci_version "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.version"}}' "$image_id" 2>/dev/null || true)" '^[a-zA-Z0-9._+-]{1,100}$'
  else
    printf 'docker.image_metadata=missing_or_inaccessible\n'
  fi

  if [[ "$running" == true ]] && response=$(docker exec "$container" wget -qO- --timeout=5 http://localhost:8080/api/v1/version 2>/dev/null) && (( ${#response} <= 4096 )); then
    printf 'endpoint.version_http=success\n'
    field endpoint.version "$(json_string "$response" version)" '^[a-zA-Z0-9._+-]{1,100}$'
    field endpoint.revision "$(json_string "$response" revision)" '^[a-zA-Z0-9._+-]{1,100}$'
  else
    printf 'endpoint.version_http=unavailable_or_non_2xx\n'
  fi
  if [[ "$running" == true ]] && response=$(docker exec "$container" wget -qO- --timeout=5 http://localhost:8080/api/v1/ready 2>/dev/null) && (( ${#response} <= 4096 )); then
    printf 'endpoint.ready_http=success\n'
    field endpoint.ready_status "$(json_string "$response" status)" '^[a-zA-Z0-9_-]{1,32}$'
    field endpoint.ready_database "$(json_string "$response" database)" '^[a-zA-Z0-9_-]{1,32}$'
    field endpoint.ready_storage "$(json_string "$response" storage)" '^[a-zA-Z0-9_-]{1,32}$'
  else
    printf 'endpoint.ready_http=unavailable_or_non_2xx\n'
  fi
fi

metadata=$deploy_dir/last-deploy.json
if [[ ! -f "$metadata" || -L "$metadata" ]]; then
  printf 'deploy.metadata=missing_or_symlink\n'
elif [[ $(stat -c %s "$metadata" 2>/dev/null || true) =~ ^[0-9]+$ ]] && (( $(stat -c %s "$metadata") <= 16384 )) && document=$(cat "$metadata" 2>/dev/null); then
  printf 'deploy.metadata=present\n'
  field deploy.status "$(json_string "$document" status)" '^[a-zA-Z0-9_-]{1,32}$'
  field deploy.image "$(json_string "$document" image)" '^[a-zA-Z0-9][a-zA-Z0-9._/:@+-]{0,240}$'
  field deploy.revision "$(json_string "$document" revision)" '^[a-zA-Z0-9._+-]{1,100}$'
  field deploy.running_revision "$(json_string "$document" running_revision)" '^[a-zA-Z0-9._+-]{1,100}$'
  field deploy.previous_image "$(json_string "$document" previous_image)" '^[a-zA-Z0-9][a-zA-Z0-9._/:@+-]{0,240}$'
  field deploy.previous_revision "$(json_string "$document" previous_revision)" '^[a-zA-Z0-9._+-]{1,100}$'
  field deploy.version "$(json_string "$document" version)" '^[a-zA-Z0-9._+-]{1,100}$'
  field deploy.at "$(json_string "$document" at)" '^[0-9TZ:.-]{1,40}$'
  backup_ref=$(json_string "$document" backup)
  if [[ "$backup_ref" =~ ^/opt/docker/openpost/backups/[0-9]{8}T[0-9]{6}Z/postgres[.]dump$ ]]; then
    printf 'deploy.backup_ref=%s\n' "${backup_ref#"$deploy_dir/"}"
  else
    printf 'deploy.backup_ref=missing_or_unexpected\n'
  fi
else
  printf 'deploy.metadata=unavailable_or_oversized\n'
fi

if [[ ! -d "$backup_dir" || -L "$backup_dir" ]]; then
  printf 'backups.directory=missing_or_symlink\n'
elif ! records=$(find "$backup_dir" -mindepth 2 -maxdepth 2 -type f \( -name postgres.dump -o -name result.json \) -printf '%P|%s|%TY-%Tm-%TdT%TH:%TM:%TS\n' 2>/dev/null | sort -r); then
  printf 'backups.files=unavailable\n'
else
  printf 'backups.directory=present\n'
  total=0
  shown=0
  while IFS='|' read -r relative size modified; do
    [[ -n "$relative" ]] || continue
    total=$((total + 1))
    if (( shown >= 20 )); then continue; fi
    if [[ "$relative" =~ ^[0-9]{8}T[0-9]{6}Z/(postgres[.]dump|result[.]json)$ && "$size" =~ ^[0-9]+$ && "$modified" =~ ^[0-9T:.-]+$ ]]; then
      printf 'backups.file=%s size_bytes=%s modified_utc=%s\n' "$relative" "$size" "$modified"
      shown=$((shown + 1))
    fi
  done <<< "$records"
  printf 'backups.files_total=%s\nbackups.files_shown=%s\n' "$total" "$shown"
  if (( total > shown )); then printf 'backups.files_truncated_or_omitted=true\n'; fi
fi

printf 'audit.collection_completed=true\n'
