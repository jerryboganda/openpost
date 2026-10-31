import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { cp, mkdir, mkdtemp, readFile, rm, symlink } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const bun = process.versions.bun ? process.execPath : "bun";
const env = {
  ...process.env,
  GOCACHE: process.env.GOCACHE || path.join(root, ".devenv", "state", "go-build"),
};
const generatedPaths = [
  "apps/web/openapi.json",
  "apps/docs/content/docs/guides/media-limits.mdx",
  "packages/api-contract/src/schema.d.ts",
  "docs/reference/cli.md",
  "packages/n8n-nodes-openpost/generated/selectedContract.ts",
  "packages/n8n-nodes-openpost/nodes/OpenPost/v1/actions/generated/requestMappers.ts",
  "packages/n8n-nodes-openpost/nodes/OpenPost/v1/descriptions/generated/descriptions.ts",
  "packages/n8n-nodes-openpost/docs/selected-contract-report.md",
];

// Run generators against a snapshot of current source, including uncommitted
// files. Checks never write or restore files in a shared checkout.
const snapshot = await mkdtemp(path.join(os.tmpdir(), "openpost-contracts-"));
let changed;
try {
  await copySourceSnapshot(snapshot);
  run(bun, ["scripts/sync-docs-openapi.mjs"], snapshot);
  run(bun, ["run", "--filter", "@openpost/web", "generate:types"], snapshot);
  run(bun, ["scripts/generate-selected-automation-contract.mjs"], snapshot);
  const expected = await generatedHashes(snapshot);
  const current = await generatedHashes(root);
  changed = generatedPaths.filter((file) => current.get(file) !== expected.get(file));
} finally {
  await rm(snapshot, { recursive: true, force: true });
}
if (changed.length > 0) {
  console.error(`Generated contracts are stale: ${changed.join(", ")}`);
  console.error("Run the owning generate command, review the diff, and commit the result.");
  process.exit(1);
}
console.log(
  "Generated API, shared TypeScript, docs, CLI, and selected automation contracts are current.",
);

async function copySourceSnapshot(destination) {
  const listed = spawnSync(
    "git",
    ["ls-files", "--cached", "--others", "--exclude-standard", "-z"],
    { cwd: root, encoding: "utf8", maxBuffer: 64 * 1024 * 1024 },
  );
  if (listed.error) throw listed.error;
  if (listed.status !== 0)
    throw new Error(
      `Failed to list source files for contract check (exit ${listed.status}): ${listed.stderr}`,
    );
  const files = [...new Set(listed.stdout.split("\0").filter(Boolean))];
  for (const file of files) {
    const target = path.join(destination, file);
    await mkdir(path.dirname(target), { recursive: true });
    try {
      await cp(path.join(root, file), target, { dereference: true });
    } catch (error) {
      // Tracked deletions are part of the current source snapshot.
      if (error?.code !== "ENOENT") throw error;
    }
  }
  const packageRoots = new Set([
    "",
    ...files
      .filter((file) => path.basename(file) === "package.json")
      .map((file) => path.dirname(file)),
  ]);
  for (const directory of packageRoots) {
    const modules = path.join(root, directory, "node_modules");
    try {
      await symlink(
        modules,
        path.join(destination, directory, "node_modules"),
        process.platform === "win32" ? "junction" : "dir",
      );
    } catch (error) {
      if (error?.code !== "EEXIST") throw error;
    }
  }
}

async function generatedHashes(directory) {
  return new Map(
    await Promise.all(
      generatedPaths.map(async (file) => {
        try {
          return [
            file,
            createHash("sha256")
              .update(await readFile(path.join(directory, file)))
              .digest("hex"),
          ];
        } catch (error) {
          if (error?.code === "ENOENT") return [file, null];
          throw error;
        }
      }),
    ),
  );
}

function run(command, args, cwd) {
  const result = spawnSync(command, args, { cwd, env, stdio: "inherit" });
  if (result.error) throw result.error;
  if (result.status !== 0)
    throw new Error(`Contract generator failed with exit code ${result.status ?? 1}`);
}
