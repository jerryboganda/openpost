import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

test("agent changes pass repository checks before publication", () => {
  const workflow = readFileSync(new URL("../.github/workflows/agent-run.yml", import.meta.url), "utf8");
  assert.match(workflow, /verify:[\s\S]*?- name: Check the proposed changes[\s\S]*?bun run check/);
  assert.match(workflow, /publish:[\s\S]*?if: needs\.verify\.result == 'success'/);
  assert.match(workflow, /failure-code check_failed/);
});

test("coding engine runs in a separate job without global callback credentials", () => {
  const workflow = readFileSync(new URL("../.github/workflows/agent-run.yml", import.meta.url), "utf8");
  const engine = workflow.split("\n  engine:\n")[1].split("\n  verify:\n")[0];
  assert.doesNotMatch(engine, /OPENPOST_AGENT_CALLBACK_SECRET|GH_TOKEN|persist-credentials: true/);
  assert.match(engine, /OPENPOST_AGENT_READ_CAPABILITY_DIR/);
  assert.match(workflow, /prepare:[\s\S]*?agent-read-capability\.mjs/);
  assert.match(workflow, /for operation in accounts posts schedules analytics workflows workflow-runs/);
  assert.match(workflow, /agent-read\.mjs workflow-runs \[cursor\]/);
  assert.match(workflow, /report:[\s\S]*?OPENPOST_AGENT_CALLBACK_SECRET/);
});

test("VPS deployment is never triggered automatically while baseline is unverified", () => {
  const workflow = readFileSync(new URL("../.github/workflows/fork-release.yml", import.meta.url), "utf8");
  assert.doesNotMatch(workflow, /^\s{2}push:/m);
  assert.match(workflow, /default: build-only/);
  assert.match(workflow, /false && always\(\)/);
});
