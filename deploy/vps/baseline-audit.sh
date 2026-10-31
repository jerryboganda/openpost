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
  if [[ "$path" == *'/../'* || "$path" == *'/./'* || "$path" == */.. || "$path" == */. ]]; then
    printf '[redacted]'
    return
  fi
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

path_field() {
  local name=$1 value=${2:-}
  if [[ -z "$value" || "$value" == '<no value>' ]]; then
    printf '%s=missing\n' "$name"
  else
    printf '%s=%s\n' "$name" "$(safe_path "$value")"
  fi
}

permitted_project_directory() {
  local path=$1
  [[ "$path" == "$deploy_dir" || "$path" == "$deploy_dir/"* ]] || return 1
  [[ "$path" =~ ^(/[a-zA-Z0-9_.@+-]+)+$ ]] || return 1
  [[ "$(safe_path "$path")" == "$path" ]] || return 1
  [[ -d "$path" && ! -L "$path" ]] || return 1
  [[ "$(realpath -e -- "$path" 2>/dev/null)" == "$path" ]]
}

inspect_checkout() {
  local directory=$1 root head status total shown line path index_status worktree_status
  if [[ -L "$directory/.git" || -f "$directory/.git" ]]; then
    printf 'source.checkout=gitfile_or_symlink_not_inspected\n'
  elif [[ ! -d "$directory/.git" || "$(realpath -e -- "$directory/.git" 2>/dev/null)" != "$directory/.git" ]]; then
    printf 'source.checkout=missing_or_untrusted_git_directory\n'
  elif ! command -v git >/dev/null 2>&1; then
    printf 'source.checkout=git_unavailable\n'
  else
    root=$(GIT_CEILING_DIRECTORIES="$(dirname "$directory")" GIT_OPTIONAL_LOCKS=0 git -C "$directory" rev-parse --show-toplevel 2>/dev/null || true)
    if [[ "$root" != "$directory" ]]; then
      printf 'source.checkout=unexpected_root_or_unreadable\n'
      return
    fi
    printf 'source.checkout=present\n'
    head=$(GIT_OPTIONAL_LOCKS=0 git -C "$directory" rev-parse --verify HEAD 2>/dev/null || true)
    field source.head "$head" '^[0-9a-f]{40}$'
    if status=$(GIT_OPTIONAL_LOCKS=0 GIT_CEILING_DIRECTORIES="$(dirname "$directory")" \
      git -C "$directory" -c core.fsmonitor=false -c core.untrackedCache=false -c core.quotePath=true \
      status --porcelain=v1 --untracked-files=all 2>/dev/null | awk 'NR <= 151 { print }'); then
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
      printf 'source.status_entries_sampled=%s\nsource.status_shown=%s\n' "$total" "$shown"
      if (( total > shown )); then printf 'source.status_truncated=true\n'; fi
      if (( total == 0 )); then printf 'source.clean=true\n'; else printf 'source.clean=false\n'; fi
    else
      printf 'source.status=unavailable\n'
    fi
  fi
}

scan_git_markers() {
  local directory=$1 entry seen=0 found=0 shown=0
  while IFS= read -r -d '' entry; do
    if (( seen >= 200 )); then
      printf 'source.git_marker_scan_truncated=true\n'
      break
    fi
    seen=$((seen + 1))
    if [[ "${entry##*/}" == .git ]]; then
      found=$((found + 1))
      if (( shown < 8 )); then
        printf 'source.git_marker=%s\n' "$(safe_path "${entry#"$directory"/}")"
        shown=$((shown + 1))
      fi
    fi
  done < <(find -P "$directory" -xdev -mindepth 1 -maxdepth 3 \
    \( -name .git -print0 -prune \) -o \
    \( -type d \( -name backups -o -name node_modules -o -name .ssh -o -name .env \
      -o -iname '*secret*' -o -iname '*credential*' -o -iname '*private*' -o -iname '*token*' \) -prune \) -o \
    -print0 2>/dev/null)
  printf 'source.git_marker_scan_depth=3\nsource.git_marker_scan_entry_limit=200\n'
  printf 'source.git_marker_entries_scanned=%s\nsource.git_markers_found=%s\n' "$seen" "$found"
  if (( found > shown )); then printf 'source.git_markers_truncated=true\n'; fi
}

printf 'audit.scope=read_only_openpost_container_and_permitted_project_tree\n'
printf 'audit.collected_utc=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
printf 'source.expected_directory=%s\n' "$deploy_dir"

if ! command -v docker >/dev/null 2>&1; then
  printf 'docker=command_missing\n'
  printf 'source.checkout=not_inspected_no_compose_metadata\n'
elif ! details=$(docker container inspect --format '{{.Name}}|{{.Id}}|{{.Config.Image}}|{{.Image}}|{{.State.Running}}' "$container" 2>/dev/null); then
  printf 'docker.container=missing_or_inaccessible\n'
  printf 'source.checkout=not_inspected_no_compose_metadata\n'
else
  IFS='|' read -r name container_id image_ref image_id running <<< "$details"
  field docker.container_name "$name" '^/[a-zA-Z0-9_.-]{1,80}$'
  field docker.container_id "$container_id" '^[0-9a-f]{64}$'
  field docker.image_ref "$image_ref" '^[a-zA-Z0-9][a-zA-Z0-9._/:@+-]{0,240}$'
  field docker.image_id "$image_id" '^sha256:[0-9a-f]{64}$'
  field docker.running "$running" '^(true|false)$'

  # Only the four Compose identity/location labels on the known OpenPost container are read.
  compose_project=$(docker container inspect --format '{{index .Config.Labels "com.docker.compose.project"}}' "$container" 2>/dev/null || true)
  compose_service=$(docker container inspect --format '{{index .Config.Labels "com.docker.compose.service"}}' "$container" 2>/dev/null || true)
  compose_directory=$(docker container inspect --format '{{index .Config.Labels "com.docker.compose.project.working_dir"}}' "$container" 2>/dev/null || true)
  compose_configs=$(docker container inspect --format '{{index .Config.Labels "com.docker.compose.project.config_files"}}' "$container" 2>/dev/null || true)
  field docker.compose_project "$compose_project" '^[a-z0-9][a-z0-9_.-]{0,79}$'
  field docker.compose_service "$compose_service" '^[a-z0-9][a-z0-9_.-]{0,79}$'
  path_field docker.compose_working_dir "$compose_directory"

  config_inside_directory=false
  if [[ ${#compose_configs} -le 2048 && "$compose_configs" =~ ^[/a-zA-Z0-9_.,@+-]+$ ]]; then
    IFS=',' read -r -a config_paths <<< "$compose_configs"
    for config_path in "${config_paths[@]:0:8}"; do
      path_field docker.compose_config_file "$config_path"
      if [[ "$config_path" == "$compose_directory/"* && "$(safe_path "$config_path")" == "$config_path" ]]; then
        config_inside_directory=true
      fi
    done
    if (( ${#config_paths[@]} > 8 )); then printf 'docker.compose_config_files_truncated=true\n'; fi
  else
    printf 'docker.compose_config_files=missing_or_redacted\n'
  fi

  checkout_inspected=false
  verified_compose_directory=''
  if [[ "$compose_directory" != "$deploy_dir" && "$compose_directory" != "$deploy_dir/"* ]]; then
    printf 'source.compose_directory=outside_permitted_project_tree\n'
  elif [[ "$compose_service" != openpost || ! "$compose_project" =~ ^[a-z0-9][a-z0-9_.-]{0,79}$ || "$config_inside_directory" != true ]]; then
    printf 'source.compose_directory=unverified_compose_identity_or_config\n'
  elif permitted_project_directory "$compose_directory"; then
    printf 'source.compose_directory=verified_under_permitted_project_tree\n'
    verified_compose_directory=$compose_directory
    inspect_checkout "$compose_directory"
    scan_git_markers "$compose_directory"
    checkout_inspected=true
  else
    printf 'source.compose_directory=missing_symlink_or_noncanonical_directory\n'
  fi
  if [[ "$checkout_inspected" != true ]]; then
    printf 'source.checkout=not_inspected_unverified_compose_directory\n'
  fi

  # Mount information is metadata only. External host paths and sensitive names stay hidden.
  if mount_rows=$(docker container inspect --format '{{range .Mounts}}{{printf "%s|%s|%s|%t\n" .Type .Source .Destination .RW}}{{end}}' "$container" 2>/dev/null); then
    mount_count=0
    while IFS='|' read -r mount_type mount_source mount_destination mount_rw; do
      [[ -n "$mount_type" ]] || continue
      if (( mount_count >= 12 )); then
        printf 'docker.mounts_truncated=true\n'
        break
      fi
      mount_count=$((mount_count + 1))
      if [[ "$mount_type" != bind && "$mount_type" != volume && "$mount_type" != tmpfs ]]; then
        printf 'docker.mount[%s].type=redacted\n' "$mount_count"
        continue
      fi
      printf 'docker.mount[%s].type=%s\n' "$mount_count" "$mount_type"
      if [[ -n "$verified_compose_directory" &&
            ( "$mount_source" == "$verified_compose_directory" || "$mount_source" == "$verified_compose_directory/"* ) &&
            "$(safe_path "$mount_source")" == "$mount_source" ]]; then
        path_field "docker.mount[$mount_count].source" "$mount_source"
      else
        printf 'docker.mount[%s].source=[redacted_or_outside_permitted_tree]\n' "$mount_count"
      fi
      path_field "docker.mount[$mount_count].destination" "$mount_destination"
      field "docker.mount[$mount_count].rw" "$mount_rw" '^(true|false)$'
    done <<< "$mount_rows"
    printf 'docker.mounts_sampled=%s\n' "$mount_count"
  else
    printf 'docker.mounts=unavailable\n'
  fi

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
