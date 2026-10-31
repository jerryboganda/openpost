import { spawn } from "node:child_process";
import lockfile from "proper-lockfile";
import { fileURLToPath } from "node:url";
const packageRoot = fileURLToPath(new URL("..", import.meta.url));
// Root checks and the public-site build use separate Turbo processes.
// Allow ten minutes for the output owner on slow build hosts.
const release = await lockfile.lock(packageRoot, {
  retries: { retries: 1200, factor: 1, minTimeout: 500, maxTimeout: 500 },
});
try {
  // The lockfile heartbeat must keep running while the child writes outputs.
  await new Promise((resolve, reject) => {
    const child = spawn("svelte-package", {
      cwd: packageRoot,
      stdio: "inherit",
      shell: process.platform === "win32",
    });
    child.once("error", reject);
    child.once("close", (code, signal) => {
      if (code === 0) return resolve();
      reject(new Error(`svelte-package failed with ${signal ?? `exit code ${code}`}`));
    });
  });
} finally {
  await release();
}
