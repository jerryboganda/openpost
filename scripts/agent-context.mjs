#!/usr/bin/env node
// Fetches private run context, including bounded earlier conversation turns.
// Neither prompts nor history appear in dispatch inputs or public run metadata.
import { createHmac } from "node:crypto";
import { writeFile } from "node:fs/promises";

function option(name) {
  const index = process.argv.indexOf(name);
  return index >= 0 ? process.argv[index + 1] : undefined;
}

const url = option("--url");
const runID = option("--run-id") ?? process.env.OPENPOST_AGENT_RUN_ID;
const output = option("--output");
const secret = process.env.OPENPOST_AGENT_CALLBACK_SECRET ?? "";
if (!url || !runID || !secret) {
  console.error("context requires --url, --run-id, and OPENPOST_AGENT_CALLBACK_SECRET");
  process.exit(2);
}

const timestamp = new Date().toISOString();
const signature = `sha256=${createHmac("sha256", secret).update(`${timestamp}.${runID}`).digest("hex")}`;
const response = await fetch(url, {
  headers: {
    "X-OpenPost-Agent-Timestamp": timestamp,
    "X-OpenPost-Agent-Signature": signature,
  },
});
if (!response.ok) {
  console.error(`context request failed with ${response.status}`);
  process.exit(1);
}
const body = await response.text();
const parsed = JSON.parse(body);
for (const key of ["run_id", "engine", "prompt"]) {
  if (typeof parsed[key] !== "string" || parsed[key].length === 0) {
    console.error(`context is missing ${key}`);
    process.exit(1);
  }
}
if (
  parsed.run_id !== runID ||
  typeof parsed.workspace_id !== "string" ||
  !Array.isArray(parsed.history) ||
  parsed.history.length > 19 ||
  Buffer.byteLength(JSON.stringify(parsed.history)) > 16 * 1024 + 1 ||
  !parsed.history.every(
    (message) =>
      message &&
      ["user", "assistant"].includes(message.role) &&
      typeof message.content === "string" &&
      Buffer.byteLength(JSON.stringify(message)) <= 4 * 1024,
  )
) {
  console.error("context has invalid run, workspace, or conversation history");
  process.exit(1);
}
if (output) {
  await writeFile(output, `${JSON.stringify(parsed, null, 2)}\n`, { mode: 0o600 });
} else {
  process.stdout.write(body);
}
