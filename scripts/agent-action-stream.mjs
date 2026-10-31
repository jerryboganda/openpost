#!/usr/bin/env node
// Consume structured engine stdout without echoing prompts, commands or results.
import { createInterface } from "node:readline";
import { appendFileSync } from "node:fs";
import { actionsFromEngineEvent, maxAgentActions } from "./agent-actions.mjs";

const engine = process.argv[2];
let count = 0;
for await (const line of createInterface({ input: process.stdin })) {
  if (count >= maxAgentActions || line.length > 65536) continue;
  let event;
  try {
    event = JSON.parse(line);
  } catch {
    continue;
  }
  for (const message of actionsFromEngineEvent(engine, event)) {
    if (count >= maxAgentActions) break;
    count += 1;
    if (process.env.OPENPOST_AGENT_ACTION_FILE) {
      appendFileSync(process.env.OPENPOST_AGENT_ACTION_FILE, `${message}\n`);
    }
  }
}
