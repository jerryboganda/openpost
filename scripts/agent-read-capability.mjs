#!/usr/bin/env node
// Trusted preparation step: ask OpenPost to mint one run-bound read credential.
import { createHmac } from "node:crypto";
import { mkdir, writeFile } from "node:fs/promises";
import { dirname } from "node:path";

function option(name) {
  const index = process.argv.indexOf(name);
  return index >= 0 ? process.argv[index + 1] : undefined;
}

const callbackURL = process.env.OPENPOST_AGENT_CALLBACK_URL;
const secret = process.env.OPENPOST_AGENT_CALLBACK_SECRET;
const runID = process.env.OPENPOST_AGENT_RUN_ID;
const githubRunID = Number(process.env.GITHUB_RUN_ID);
const output = option("--output");
const operation = option("--operation") || "accounts";
const write =
  operation === "workflow-create" ||
  operation === "workflow-update" ||
  operation.startsWith("publication-");
if (
  !callbackURL ||
  !secret ||
  !runID ||
  !output ||
  ![
    "accounts",
    "posts",
    "schedules",
    "analytics",
    "workflows",
    "workflow-runs",
    "workflow-create",
    "workflow-update",
    "publication-create",
    "publication-update",
    "publication-schedule",
    "publication-reschedule",
    "publication-publish-now",
  ].includes(operation) ||
  !Number.isSafeInteger(githubRunID) ||
  githubRunID <= 0
) {
  console.error("read capability issuance is unavailable");
  process.exit(2);
}
const base = new URL(callbackURL);
if (base.protocol !== "https:" || !base.pathname.endsWith("/callback")) {
  console.error("read capability issuance requires the HTTPS callback endpoint");
  process.exit(2);
}
const url = new URL(
  `${base.pathname.replace(/\/callback$/, "")}/runs/${encodeURIComponent(runID)}/${write ? (operation.startsWith("publication-") ? "publication-write-capability" : "workflow-write-capability") : "read-capability"}`,
  base,
);
const body = JSON.stringify({ github_run_id: githubRunID, operation });
const timestamp = new Date().toISOString();
const signed = `POST ${url.pathname}\n${body}`;
const signature = `sha256=${createHmac("sha256", secret).update(`${timestamp}.${signed}`).digest("hex")}`;
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
  console.error(`read capability issuance failed with HTTP ${response.status}`);
  process.exit(1);
}
const { capability } = await response.json();
if (typeof capability !== "string" || capability.length === 0 || capability.length > 1024) {
  console.error("read capability issuance returned invalid data");
  process.exit(1);
}
await mkdir(dirname(output), { recursive: true });
await writeFile(output, `${capability}\n`, { mode: 0o600 });
