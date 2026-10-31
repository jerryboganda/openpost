// Only names and state transitions from structured engine events are audit-safe.
// Never return prompts, tool arguments, file paths, model text, or tool output.
export const maxAgentActions = 64;

const toolLabels = {
  bash: "Command",
  command: "Command",
  shell: "Command",
  read: "File read",
  write: "File write",
  edit: "File edit",
  multiedit: "File edit",
  apply_patch: "File edit",
  grep: "Repository search",
  glob: "File search",
  webfetch: "Web fetch",
  websearch: "Web search",
};

function toolLabel(name) {
  return toolLabels[typeof name === "string" ? name.toLowerCase() : ""] ?? "Tool call";
}

export function actionsFromEngineEvent(engine, event) {
  if (!event || typeof event !== "object") return [];

  if (engine === "codex") {
    const state = {
      "item.started": "started",
      "item.completed": "finished",
      "item.failed": "failed",
    }[event.type];
    const label = {
      command_execution: "Command",
      file_change: "File change",
      mcp_tool_call: "Tool call",
      web_search: "Web search",
    }[event.item?.type];
    return state && label ? [`${label} ${state}`] : [];
  }

  if (engine === "opencode" && event.type === "tool" && event.part?.type === "tool") {
    const state = {
      running: "started",
      completed: "finished",
      error: "failed",
    }[event.part.state?.status];
    return state ? [`${toolLabel(event.part.tool)} ${state}`] : [];
  }

  if (engine === "claude" && event.type === "assistant" && Array.isArray(event.message?.content)) {
    return event.message.content
      .filter((part) => part?.type === "tool_use")
      .map((part) => `${toolLabel(part.name)} requested`);
  }

  return [];
}
