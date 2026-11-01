#!/usr/bin/env node
import { readFile } from "node:fs/promises";

const [operation, source, publicationID, actionKey] = process.argv.slice(2);
const create = operation === "create";
const scheduling = operation === "schedule" || operation === "reschedule";
const publishNow = operation === "publish-now";
if (
  !(create || operation === "update" || scheduling || publishNow) ||
  !source ||
  !/^[a-zA-Z0-9_-]{8,100}$/.test(actionKey ?? "") ||
  (create ? publicationID !== "-" : !/^[a-zA-Z0-9-]{1,100}$/.test(publicationID ?? "")) ||
  process.argv.length !== 6
) {
  console.error(
    "usage: node scripts/agent-publication-write.mjs create|update|schedule|reschedule|publish-now input.json publication-id|- stable-action-key",
  );
  process.exit(2);
}
const runID = process.env.OPENPOST_AGENT_RUN_ID;
const callbackURL = process.env.OPENPOST_AGENT_CALLBACK_URL;
const directory = process.env.OPENPOST_AGENT_WRITE_CAPABILITY_DIR;
if (!runID || !callbackURL || !directory) {
  console.error("publication draft write unavailable");
  process.exit(2);
}
const base = new URL(callbackURL);
if (base.protocol !== "https:" || !base.pathname.endsWith("/callback")) {
  console.error("publication draft write requires HTTPS");
  process.exit(2);
}
const capability = (await readFile(`${directory}/publication-${operation}`, "utf8")).trim();
if (!capability || capability.length > 1024) {
  console.error("publication draft write capability unavailable");
  process.exit(2);
}
const data = await readFile(source, "utf8");
if (Buffer.byteLength(data) > 256 * 1024) {
  console.error("publication draft too large");
  process.exit(2);
}
const body = JSON.parse(data);
if (
  !body ||
  typeof body !== "object" ||
  Array.isArray(body) ||
  (!create && !(body.expected_revision > 0)) ||
  (publishNow && Object.keys(body).some((key) => key !== "expected_revision")) ||
  (scheduling &&
    (Object.keys(body).some((key) => !["expected_revision", "scheduled_at"].includes(key)) ||
      typeof body.scheduled_at !== "string" ||
      !Number.isFinite(Date.parse(body.scheduled_at)) ||
      Date.parse(body.scheduled_at) <= Date.now()))
) {
  console.error(
    "native publication body required; immediate publishing needs only expected_revision",
  );
  process.exit(2);
}
const suffix = create ? "" : `/${encodeURIComponent(publicationID)}`;
const url = new URL(
  `${base.pathname.replace(/\/callback$/, "")}/runs/${encodeURIComponent(runID)}/write/publications${suffix}${scheduling || publishNow ? `/${operation}` : ""}`,
  base,
);
const response = await fetch(url, {
  method: create || scheduling || publishNow ? "POST" : "PUT",
  headers: {
    Authorization: `Bearer ${capability}`,
    "Content-Type": "application/json",
    "Idempotency-Key": actionKey,
  },
  body: data,
});
if (!response.ok) {
  console.error(`publication ${operation} failed with HTTP ${response.status}`);
  process.exit(1);
}
const result = await response.json();
process.stdout.write(
  JSON.stringify({
    id: result.id,
    revision: result.revision,
    job_id: result.job_id,
    publication_status: result.publication_status,
    job_status: result.job_status,
    outcome: result.outcome,
    replayed: !!result.replayed,
  }) + "\n",
);
