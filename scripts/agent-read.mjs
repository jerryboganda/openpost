#!/usr/bin/env node
// Fixed read-only operation with a narrow bearer capability issued by OpenPost.
import { readFile } from "node:fs/promises";

const [operation] = process.argv.slice(2);
if (!["accounts", "posts", "schedules", "analytics", "workflows", "workflow-runs"].includes(operation) || process.argv.length > 4 || (process.argv.length === 4 && operation === "accounts")) {
  console.error("supported read-only operations: accounts; posts [cursor]; schedules [cursor]; analytics [cursor]; workflows [cursor]; workflow-runs [cursor]");
  process.exit(2);
}
const runID = process.env.OPENPOST_AGENT_RUN_ID;
const callbackURL = process.env.OPENPOST_AGENT_CALLBACK_URL;
const capabilityDirectory = process.env.OPENPOST_AGENT_READ_CAPABILITY_DIR;
const cursor = process.argv[3];
if (!runID || !callbackURL || !capabilityDirectory || (cursor && (cursor.length > 160 || !/^[^\s]+\|[^\s|]+$/.test(cursor)))) {
  console.error("read operation is unavailable");
  process.exit(2);
}
const base = new URL(callbackURL);
if (base.protocol !== "https:" || !base.pathname.endsWith("/callback")) {
  console.error("read operation requires HTTPS");
  process.exit(2);
}
const url = new URL(`${base.pathname.replace(/\/callback$/, "")}/runs/${encodeURIComponent(runID)}/read/${operation}`, base);
if (cursor) url.searchParams.set("cursor", cursor);
const capability = (await readFile(`${capabilityDirectory}/${operation}`, "utf8")).trim();
if (!capability || capability.length > 1024) {
  console.error("read capability is unavailable");
  process.exit(2);
}
const response = await fetch(url, {
  headers: {
    Authorization: `Bearer ${capability}`,
  },
});
if (!response.ok) {
  console.error(`read operation failed with HTTP ${response.status}`);
  process.exit(1);
}
process.stdout.write(`${await response.text()}\n`);
