#!/usr/bin/env node
// Posts signed agent progress to OpenPost. Runs inside GitHub Actions only.
// The callback secret is read from the environment and never printed.
import { createHmac } from "node:crypto";
import { readFile, stat } from "node:fs/promises";

function option(name) {
  const index = process.argv.indexOf(name);
  return index >= 0 ? process.argv[index + 1] : undefined;
}

function options(name) {
  const values = [];
  for (let index = 0; index < process.argv.length; index += 1) {
    if (process.argv[index] === name && process.argv[index + 1])
      values.push(process.argv[index + 1]);
  }
  return values;
}

const url = option("--url") ?? process.env.OPENPOST_AGENT_CALLBACK_URL;
const secret = process.env.OPENPOST_AGENT_CALLBACK_SECRET ?? "";
const runID = option("--run-id") ?? process.env.OPENPOST_AGENT_RUN_ID;
const status = option("--status") ?? "running";
const githubRunID = process.env.GITHUB_RUN_ID ? Number(process.env.GITHUB_RUN_ID) : undefined;
if (!url || !secret || !runID) {
  console.error("callback requires --url, --run-id, and OPENPOST_AGENT_CALLBACK_SECRET");
  process.exit(2);
}
if (status !== "running" && status !== "succeeded" && status !== "failed") {
  console.error(`unsupported callback status: ${status}`);
  process.exit(2);
}
if (githubRunID !== undefined && (!Number.isSafeInteger(githubRunID) || githubRunID <= 0)) {
  console.error("callback requires a valid GITHUB_RUN_ID");
  process.exit(2);
}

const events = options("--event").map((value) => {
  const [key, kind, ...rest] = value.split("|");
  return { key, kind: kind || "progress", message: rest.join("|") };
});
const actionFile = option("--action-file");
if (actionFile) {
  try {
    const info = await stat(actionFile);
    if (info.size <= 16 * 1024) {
      const lines = (await readFile(actionFile, "utf8")).split("\n");
      const safeAction = /^(?:Command|File read|File write|File edit|File change|Repository search|File search|Web fetch|Web search|Tool call) (?:started|finished|failed|requested)$/;
      for (const message of lines.filter((line) => safeAction.test(line)).slice(0, 64)) {
        const key = `action-${String(events.length + 1).padStart(3, "0")}`;
        events.push({ key, kind: "action", message });
      }
    }
  } catch {
    // The engine may fail before writing any actions. Status reporting continues.
  }
}
const payload = {
  run_id: runID,
  ...(githubRunID === undefined ? {} : { github_run_id: githubRunID }),
  status,
  summary: option("--summary") ?? "",
  failure_code: option("--failure-code") ?? "",
  failure_message: option("--failure-message") ?? "",
  branch: option("--branch") ?? "",
  base_revision: option("--base-revision") ?? process.env.GITHUB_SHA ?? "",
  events,
};
const body = JSON.stringify(payload);
const timestamp = new Date().toISOString();
const signature = `sha256=${createHmac("sha256", secret).update(`${timestamp}.${body}`).digest("hex")}`;

const response = await fetch(url, {
  method: "POST",
  headers: {
    "Content-Type": "application/json",
    "X-OpenPost-Agent-Timestamp": timestamp,
    "X-OpenPost-Agent-Signature": signature,
  },
  body,
});
if (!response.ok) {
  const text = (await response.text()).slice(0, 500);
  console.error(`callback rejected with ${response.status}: ${text}`);
  process.exit(1);
}
console.log(`callback accepted (${status})`);
