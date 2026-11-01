#!/usr/bin/env node
// Write only a native workflow draft with a run-bound operation capability.
import { readFile } from "node:fs/promises";

const [operation, source, workflowID, actionKey] = process.argv.slice(2);
const create = operation === "create";
if (
  !(create || operation === "update") ||
  !source ||
  !actionKey ||
  !/^[a-zA-Z0-9_-]{8,100}$/.test(actionKey) ||
  (create ? workflowID !== "-" : !/^[a-zA-Z0-9-]{1,100}$/.test(workflowID ?? "")) ||
  process.argv.length !== 6
) {
  console.error(
    "usage: node scripts/agent-workflow-write.mjs create|update definition.json workflow-id|- stable-action-key",
  );
  process.exit(2);
}
const runID = process.env.OPENPOST_AGENT_RUN_ID;
const callbackURL = process.env.OPENPOST_AGENT_CALLBACK_URL;
const directory = process.env.OPENPOST_AGENT_WRITE_CAPABILITY_DIR;
if (!runID || !callbackURL || !directory) {
  console.error("workflow write unavailable");
  process.exit(2);
}
const base = new URL(callbackURL);
if (base.protocol !== "https:" || !base.pathname.endsWith("/callback")) {
  console.error("workflow write requires HTTPS");
  process.exit(2);
}
const capability = (await readFile(`${directory}/workflow-${operation}`, "utf8")).trim();
if (!capability || capability.length > 1024) {
  console.error("workflow write capability unavailable");
  process.exit(2);
}
const data = await readFile(source, "utf8");
if (Buffer.byteLength(data) > 256 * 1024) {
  console.error("workflow definition too large");
  process.exit(2);
}
const parsed = JSON.parse(data);
if (!parsed || !parsed.definition || !Array.isArray(parsed.definition.steps)) {
  console.error("structured workflow definition required");
  process.exit(2);
}
const suffix = create ? "" : `/${encodeURIComponent(workflowID)}`;
const url = new URL(
  `${base.pathname.replace(/\/callback$/, "")}/runs/${encodeURIComponent(runID)}/write/workflows${suffix}`,
  base,
);
const response = await fetch(url, {
  method: create ? "POST" : "PUT",
  headers: {
    Authorization: `Bearer ${capability}`,
    "Content-Type": "application/json",
    "Idempotency-Key": actionKey,
  },
  body: data,
});
if (!response.ok) {
  console.error(`workflow ${operation} failed with HTTP ${response.status}`);
  process.exit(1);
}
const result = await response.json();
process.stdout.write(
  JSON.stringify({
    id: result.id,
    revision: result.revision,
    outcome: result.outcome,
    replayed: !!result.replayed,
  }) + "\n",
);
