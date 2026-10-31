import { fileURLToPath } from "node:url";

if (process.env.OPENPOST_CHECK_PREPARED !== "1") {
  const child = Bun.spawn([process.execPath, "run", "generate:i18n"], {
    cwd: fileURLToPath(new URL("../apps/web/", import.meta.url)),
    stdin: "ignore",
    stdout: "inherit",
    stderr: "inherit",
  });
  process.exitCode = await child.exited;
}
