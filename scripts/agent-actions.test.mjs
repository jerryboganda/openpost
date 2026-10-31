import assert from "node:assert/strict";
import test from "node:test";
import { actionsFromEngineEvent } from "./agent-actions.mjs";

test("recognized engine events expose tool kind and state without sensitive payloads", () => {
  const secret = "sk-private-from-model-output";
  const cases = [
    ["codex", { type: "item.started", item: { type: "command_execution", command: `echo ${secret}` } }, ["Command started"]],
    ["codex", { type: "item.completed", item: { type: "file_change", changes: [{ path: secret }] } }, ["File change finished"]],
    ["opencode", { type: "tool", part: { type: "tool", tool: "bash", state: { status: "completed", output: secret } } }, ["Command finished"]],
    ["claude", { type: "assistant", message: { content: [{ type: "tool_use", name: "Edit", input: { file_path: secret } }] } }, ["File edit requested"]],
    ["claude", { type: "assistant", message: { content: [{ type: "text", text: secret }] } }, []],
    ["codex", { type: "item.completed", item: { type: "agent_message", text: secret } }, []],
  ];
  for (const [engine, event, expected] of cases) {
    const actions = actionsFromEngineEvent(engine, event);
    assert.deepEqual(actions, expected);
    assert.doesNotMatch(JSON.stringify(actions), /sk-private-from-model-output/);
  }
});
