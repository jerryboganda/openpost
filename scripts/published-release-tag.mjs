#!/usr/bin/env bun

import { fileURLToPath } from "node:url";

export function selectPublishedStableRelease(releasePages, excludeTag) {
  return releasePages
    .flat()
    .filter(
      (release) =>
        !release.draft &&
        !release.prerelease &&
        release.tag_name !== excludeTag &&
        // GitHub API JSON is untyped until this boundary validates it.
        // oxlint-disable-next-line anti-slop/no-runtime-typeof
        typeof release.published_at === "string",
    )
    .sort((left, right) => right.published_at.localeCompare(left.published_at))[0]?.tag_name;
}

// Forks publish no GitHub releases, so the release baseline falls back to the
// newest stable version tag reachable from HEAD.
export function selectReachableStableTag(tagLines, excludeTag) {
  return tagLines
    .map((line) => line.trim())
    .find((tag) => /^v\d+\.\d+\.\d+$/u.test(tag) && tag !== excludeTag);
}

function reachableStableTag(excludeTag) {
  const result = Bun.spawnSync([
    "git",
    "tag",
    "--merged",
    "HEAD",
    "--list",
    "v[0-9]*",
    "--sort=-v:refname",
  ]);
  if (result.exitCode !== 0) return undefined;
  return selectReachableStableTag(result.stdout.toString().split("\n"), excludeTag);
}

export function publishedStableReleaseTag({ excludeTag } = {}) {
  const result = Bun.spawnSync([
    "gh",
    "api",
    "--paginate",
    "--slurp",
    "repos/{owner}/{repo}/releases?per_page=100",
  ]);
  if (result.exitCode !== 0) {
    throw new Error(`could not list GitHub releases: ${result.stderr.toString().trim()}`);
  }
  const tag =
    selectPublishedStableRelease(JSON.parse(result.stdout.toString()), excludeTag) ??
    reachableStableTag(excludeTag);
  if (!tag) throw new Error("no published stable GitHub release found");
  return tag;
}

function option(name) {
  const index = process.argv.indexOf(name);
  return index >= 0 ? process.argv[index + 1] : undefined;
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  process.stdout.write(`${publishedStableReleaseTag({ excludeTag: option("--exclude") })}\n`);
}
