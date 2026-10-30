import { execFileSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import http from "node:http";
import type { AddressInfo } from "node:net";
import zlib from "node:zlib";
import { expect, test, type Page, type TestInfo } from "@playwright/test";
import { authenticatePage, createWorkspace, registerUser } from "./helpers";

type Json = Record<string, unknown>;

function crc32(buffer: Buffer): number {
  let crc = 0xffffffff;
  for (const byte of buffer) {
    crc ^= byte;
    for (let bit = 0; bit < 8; bit += 1) crc = crc & 1 ? 0xedb88320 ^ (crc >>> 1) : crc >>> 1;
  }
  return (crc ^ 0xffffffff) >>> 0;
}

function pngChunk(type: string, data: Buffer): Buffer {
  const length = Buffer.alloc(4);
  length.writeUInt32BE(data.length);
  const body = Buffer.concat([Buffer.from(type), data]);
  const checksum = Buffer.alloc(4);
  checksum.writeUInt32BE(crc32(body));
  return Buffer.concat([length, body, checksum]);
}

/** A small screenshot-like picture: a title bar over a page with one panel. */
function screenshotPng(width: number, height: number): Buffer {
  const rows = Buffer.alloc((width * 3 + 1) * height);
  for (let y = 0; y < height; y += 1) {
    const row = y * (width * 3 + 1);
    for (let x = 0; x < width; x += 1) {
      const inPanel =
        x > width * 0.15 && x < width * 0.85 && y > height * 0.35 && y < height * 0.85;
      const [red, green, blue] =
        y < height * 0.18 ? [40, 90, 200] : inPanel ? [255, 255, 255] : [230, 232, 236];
      rows[row + 1 + x * 3] = red;
      rows[row + 2 + x * 3] = green;
      rows[row + 3 + x * 3] = blue;
    }
  }
  const header = Buffer.alloc(13);
  header.writeUInt32BE(width, 0);
  header.writeUInt32BE(height, 4);
  header[8] = 8;
  header[9] = 2;
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    pngChunk("IHDR", header),
    pngChunk("IDAT", zlib.deflateSync(rows)),
    pngChunk("IEND", Buffer.alloc(0)),
  ]);
}

const PICTURE = screenshotPng(240, 140);
// Fixture times are relative to now, so elapsed times read like a real run.
const START = Date.now() - 2 * 60 * 1000;
const iso = (seconds: number) => new Date(START + seconds * 1000).toISOString();

const MODELS = [
  {
    id: "deepseek-v4.1-flash",
    label: "DeepSeek V4.1 Flash",
    description: "Fast and low cost. Reads images.",
    vision: true,
  },
  {
    id: "kimi-k3",
    label: "Kimi K3",
    description: "Strong at long multi-step tasks. Reads images.",
    vision: true,
  },
  { id: "glm-5.3", label: "GLM 5.3", description: "Text only.", vision: false },
];

const ENGINES = {
  default_model: "deepseek-v4.1-flash",
  models: MODELS,
  engines: [
    { id: "assistant", label: "OpenPost Agent", available: true },
    { id: "opencode", label: "OpenCode on GitHub Actions", available: true },
    {
      id: "codex",
      label: "Codex on GitHub Actions",
      available: false,
      reason: "Add the OPENAI_API_KEY secret.",
    },
  ],
};

const FINAL_ANSWER = [
  "## Result",
  "",
  "I fixed the login copy and checked production. **Nothing else changed.**",
  "",
  "- Updated `apps/web/messages/en.json` on `agent/fix-copy`",
  "- Production is healthy: version `v1.2.3`, database ready",
  "- The CI status call failed, so the pipeline result is unknown",
  "",
  "| Check | State |",
  "| --- | --- |",
  "| Build | passed |",
  "| Lint | pending |",
  "",
  "```ts",
  "const label = 'Sign in';",
  "```",
  "",
  "See [the pull request](https://github.com/example/openpost/pull/7) for details.",
].join("\n");

function attachment(id: string, filename: string): Json {
  return {
    id,
    filename,
    content_type: "image/png",
    size_bytes: PICTURE.length,
    width: 240,
    height: 140,
    created_at: iso(-90),
  };
}

function run(id: string, status: string, extra: Json = {}): Json {
  return {
    id,
    conversation_id: "",
    engine: "assistant",
    status,
    prompt: "prompt",
    model: "deepseek-v4.1-flash",
    effort: "auto",
    access: "full",
    created_at: iso(-60),
    started_at: iso(-60),
    ...extra,
  };
}

function event(runId: string, seq: number, kind: string, message: string, data?: Json): Json {
  return {
    id: `${runId}-e${seq}`,
    run_id: runId,
    seq,
    kind,
    message,
    data,
    created_at: iso(-60 + seq),
  };
}

function toolCall(
  runId: string,
  seq: number,
  key: string,
  name: string,
  label: string,
  args: Json,
): Json {
  return event(runId, seq, "tool_call", label, { key, name, arguments: args });
}

function toolResult(
  runId: string,
  seq: number,
  key: string,
  name: string,
  output: string,
  ok = true,
  ms = 420,
): Json {
  return event(runId, seq, "tool_result", output.split("\n")[0], { key, name, ok, output, ms });
}

function storedConversation(): Json {
  const runId = "run-history";
  const conversationId = "conv-history";
  return {
    id: conversationId,
    engine: "assistant",
    title: "Fix the login copy",
    created_at: iso(-120),
    updated_at: iso(-30),
    messages: [
      {
        id: "msg-1",
        conversation_id: conversationId,
        role: "user",
        content:
          "The sign-in button copy looks wrong in this screenshot. Fix it and tell me if production is fine.",
        run_id: runId,
        created_at: iso(-60),
        attachments: [attachment("att-history", "screenshot.png")],
      },
      {
        id: "msg-2",
        conversation_id: conversationId,
        role: "assistant",
        content: FINAL_ANSWER,
        run_id: runId,
        created_at: iso(-31),
      },
    ],
    runs: [
      run(runId, "succeeded", {
        conversation_id: conversationId,
        input_tokens: 18400,
        output_tokens: 1320,
        finished_at: iso(-31),
        summary: FINAL_ANSWER,
      }),
    ],
    events: [
      event(runId, 1, "status", "Working with DeepSeek V4.1 Flash", {
        model: "deepseek-v4.1-flash",
        tools: ["repo_read"],
      }),
      event(runId, 2, "text", "I'll compare the screenshot with the page code first."),
      toolCall(runId, 3, "1-0", "repo_search", "Search code for sign in", { query: "sign in" }),
      toolResult(
        runId,
        4,
        "1-0",
        "repo_search",
        '3 match(es) for "sign in"\n\napps/web/messages/en.json',
      ),
      toolCall(runId, 5, "2-0", "repo_edit", "Edit apps/web/messages/en.json on agent/fix-copy", {
        branch: "agent/fix-copy",
        path: "apps/web/messages/en.json",
        old_string: '  "login_button": "Signin",\n  "login_title": "Welcome back"',
        new_string: '  "login_button": "Sign in",\n  "login_title": "Welcome back"',
      }),
      toolResult(
        runId,
        6,
        "2-0",
        "repo_edit",
        "Edited apps/web/messages/en.json on agent/fix-copy: 1 replacement(s). Commit c0ffee1.",
      ),
      toolCall(runId, 7, "3-0", "db_query", "Query: SELECT COUNT(*) AS total FROM users", {
        sql: "SELECT COUNT(*) AS total FROM users",
      }),
      toolResult(runId, 8, "3-0", "db_query", "total\n12"),
      toolCall(runId, 9, "4-0", "ci_status", "List workflow runs", {}),
      toolResult(
        runId,
        10,
        "4-0",
        "ci_status",
        "Error: GitHub returned 502: Bad Gateway",
        false,
        90,
      ),
      event(runId, 11, "text", FINAL_ANSWER),
    ],
  };
}

interface Recorded {
  method: string;
  path: string;
  body: Json | null;
}

class FakeAgent {
  readonly requests: Recorded[] = [];
  conversations = new Map<string, Json>();
  uploads: { filename: string; type: string; bytes: Buffer }[] = [];
  uploadGate: Promise<void> = Promise.resolve();
  /** Answers a sent message with the run that starts, and prepares the stored conversation. */
  onMessage: (conversationId: string, body: Json | null) => Json = () => ({});
  private streams = new Map<string, http.ServerResponse[]>();
  private waiting = new Map<string, () => void>();
  private server: http.Server;
  streamURL = "";

  constructor() {
    this.server = http.createServer((request, response) => {
      const runId = new URL(request.url ?? "/", "http://stream.test").searchParams.get("run") ?? "";
      response.writeHead(200, {
        "content-type": "text/event-stream",
        "cache-control": "no-cache",
        // route.continue({ url }) swaps the target without a redirect, so the page
        // should treat this as same-origin. The header covers the case it does not.
        "access-control-allow-origin": "*",
        connection: "keep-alive",
      });
      response.write(": connected\n\n");
      this.streams.set(runId, [...(this.streams.get(runId) ?? []), response]);
      this.waiting.get(runId)?.();
    });
  }

  async start(): Promise<void> {
    await new Promise<void>((resolve) => this.server.listen(0, "127.0.0.1", resolve));
    this.streamURL = `http://127.0.0.1:${(this.server.address() as AddressInfo).port}/events`;
  }

  async stop(): Promise<void> {
    for (const responses of this.streams.values()) for (const response of responses) response.end();
    this.server.closeAllConnections();
    await new Promise<void>((resolve) => this.server.close(() => resolve()));
  }

  connected(runId: string): Promise<void> {
    if ((this.streams.get(runId) ?? []).length > 0) return Promise.resolve();
    return new Promise((resolve) => this.waiting.set(runId, resolve));
  }

  push(runId: string, name: string, data: unknown) {
    for (const response of this.streams.get(runId) ?? []) {
      response.write(`event: ${name}\ndata: ${JSON.stringify(data)}\n\n`);
    }
  }

  calls(method: string, pattern: RegExp): Recorded[] {
    return this.requests.filter(
      (request) => request.method === method && pattern.test(request.path),
    );
  }
}

async function installFakeAgent(page: Page, fake: FakeAgent) {
  await page.route(/\/api\/v1\/admin\/agent\//, async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = url.pathname.replace("/api/v1/admin/agent", "");
    const method = request.method();
    const isUpload = method === "POST" && path === "/attachments";
    let body: Json | null = null;
    if (!isUpload && request.postData()) body = JSON.parse(request.postData() ?? "null");
    fake.requests.push({ method, path, body });

    if (method === "GET" && path === "/engines") return route.fulfill({ json: ENGINES });
    if (method === "GET" && path === "/conversations") {
      const list = [...fake.conversations.values()].map((view) => ({
        id: view.id,
        engine: view.engine,
        title: view.title,
        created_at: view.created_at,
        updated_at: view.updated_at,
      }));
      return route.fulfill({ json: { conversations: list } });
    }
    if (method === "POST" && path === "/conversations") {
      const id = "conv-new";
      const view = {
        id,
        engine: body?.engine,
        title: "",
        created_at: iso(0),
        updated_at: iso(0),
        messages: null,
        runs: null,
        events: null,
      };
      fake.conversations.set(id, view);
      return route.fulfill({ json: { conversation: view } });
    }
    const conversation = /^\/conversations\/([^/]+)$/.exec(path);
    if (conversation && method === "GET") {
      const view = fake.conversations.get(conversation[1]);
      if (!view) return route.fulfill({ status: 404, json: { title: "Not Found", status: 404 } });
      return route.fulfill({ json: { conversation: view } });
    }
    if (conversation && method === "DELETE") {
      fake.conversations.delete(conversation[1]);
      return route.fulfill({ status: 204 });
    }
    const events = /^\/runs\/([^/]+)\/events$/.exec(path);
    if (events && method === "GET") {
      return route.continue({ url: `${fake.streamURL}?run=${events[1]}` });
    }
    const messages = /^\/conversations\/([^/]+)\/messages$/.exec(path);
    if (messages && method === "POST") {
      return route.fulfill({ json: fake.onMessage(messages[1], body) });
    }
    if (isUpload) {
      await fake.uploadGate;
      fake.uploads.push({
        filename: url.searchParams.get("filename") ?? "",
        type: request.headers()["content-type"] ?? "",
        bytes: request.postDataBuffer() ?? Buffer.alloc(0),
      });
      const info = attachment(
        `att-upload-${fake.uploads.length}`,
        url.searchParams.get("filename") ?? "image",
      );
      return route.fulfill({ status: 201, json: info });
    }
    if (method === "GET" && path.startsWith("/attachments/")) {
      return route.fulfill({ contentType: "image/png", body: PICTURE });
    }
    return route.fulfill({
      status: 404,
      json: { title: "Not mocked", status: 404, detail: `${method} ${path}` },
    });
  });
}

async function adminPage(page: Page, request: Parameters<typeof registerUser>[0]) {
  const auth = await registerUser(request, `agent-chat-${randomUUID()}@example.com`);
  await createWorkspace(request, auth.token, "Agent chat");
  const me = await request.get("/api/v1/auth/me", {
    headers: { Authorization: `Bearer ${auth.token}` },
  });
  expect(me.ok()).toBe(true);
  const { id } = await me.json();
  const database = `/tmp/openpost-app-e2e-${process.env.OPENPOST_APP_E2E_PORT ?? 18180}.db`;
  execFileSync("sqlite3", [
    "-cmd",
    ".timeout 5000",
    database,
    `UPDATE users SET is_admin = 1 WHERE id = '${id}';`,
  ]);
  await authenticatePage(page, auth.token);
}

async function useScheme(page: Page, scheme: "light" | "dark") {
  await page.emulateMedia({ colorScheme: scheme, reducedMotion: "reduce" });
  await page
    .evaluate((value) => localStorage.setItem("mode-watcher-mode", value), scheme)
    .catch(() => undefined);
}

async function expectNoSidewaysScroll(page: Page) {
  expect(
    await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth),
  ).toBeLessThanOrEqual(1);
}

const composerOf = (page: Page) => page.getByRole("textbox", { name: "Message for the agent" });
const transcriptOf = (page: Page) => page.getByRole("log", { name: "Conversation" });
const chatList = (page: Page) => page.getByRole("navigation", { name: "Chats" });

function escapeRegExp(text: string): string {
  return text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

// A chat row has two buttons: one that opens it and one named "Delete chat <title>".
// Anchoring at the start keeps the locator on the one that opens it.
const chatButton = (page: Page, title: string) =>
  chatList(page).getByRole("button", { name: new RegExp(`^${escapeRegExp(title)}`) });

test("a stored conversation reads like a chat: steps, a diff, formatted answer, and screenshots", async ({
  page,
  request,
}, info: TestInfo) => {
  const fake = new FakeAgent();
  await fake.start();
  try {
    fake.conversations.set("conv-history", storedConversation());
    await installFakeAgent(page, fake);
    await adminPage(page, request);
    await page.setViewportSize({ width: 1280, height: 900 });
    await useScheme(page, "light");
    await page.goto("/agent");

    await expect(page.getByRole("heading", { name: "Agent", exact: true })).toBeVisible();
    await expect(page.getByText("What should the agent work on?")).toBeVisible();
    await expect(chatButton(page, "Fix the login copy")).toBeVisible();

    await composerOf(page).focus();
    expect
      .soft(
        await composerOf(page).evaluate((element) => {
          const style = getComputedStyle(element);
          // Zero-size layers are how Tailwind writes "no shadow"; any other layer is a visible ring.
          const ring = style.boxShadow
            .split(/,\s*(?![^(]*\))/)
            .some((layer) => !/ 0px 0px 0px 0px$/.test(layer.trim()));
          return { outline: style.outlineStyle, border: style.borderTopWidth, ring };
        }),
        "the composer field draws no border, outline, or ring of its own; its frame shows focus",
      )
      .toEqual({ outline: "none", border: "0px", ring: false });

    await page.getByRole("button", { name: "Check production health" }).click();
    await expect(composerOf(page)).toHaveValue(/^Check production health\./);
    expect(fake.calls("POST", /messages$/)).toHaveLength(0);
    await page.screenshot({ path: info.outputPath("empty-1280-light.png") });

    await chatButton(page, "Fix the login copy").click();
    const transcript = transcriptOf(page);
    await expect(
      transcript.getByText("The sign-in button copy looks wrong", { exact: false }),
    ).toBeVisible();
    await expect(transcript.getByRole("button", { name: "Open screenshot.png" })).toBeVisible();
    await expect(transcript.getByRole("heading", { name: "Result" })).toBeVisible();
    await expect(transcript.getByRole("cell", { name: "passed" })).toBeVisible();
    await expect(transcript.getByRole("link", { name: "the pull request" })).toHaveAttribute(
      "href",
      "https://github.com/example/openpost/pull/7",
    );
    await expect(transcript).not.toContainText("**");
    await expect(transcript.getByText("Nothing else changed.")).toHaveCount(1);
    await expect(transcript.locator("[data-slot='agent-code']")).toContainText(
      "const label = 'Sign in';",
    );

    const search = transcript.getByRole("button", { name: /Search code for sign in/ });
    await expect(search).toHaveAttribute("aria-expanded", "false");
    const failed = transcript.getByRole("button", { name: /List workflow runs/ });
    await expect(failed).toContainText("Failed");
    await expect(failed).toHaveAttribute("aria-expanded", "true");
    await expect(transcript.getByText("Error: GitHub returned 502: Bad Gateway")).toBeVisible();

    const edit = transcript.getByRole("button", { name: /Edit apps\/web\/messages\/en.json/ });
    await edit.click();
    const diff = transcript.locator("[data-slot='agent-diff']");
    await expect(diff).toContainText("+1");
    await expect(diff).toContainText("−1");
    await expect(diff.getByText('"login_button": "Signin"')).toBeVisible();
    await expect(diff.getByText('"login_button": "Sign in"')).toBeVisible();
    await expect(transcript.getByText("DeepSeek V4.1 Flash")).toBeVisible();
    await expect(transcript.getByRole("button", { name: "Copy reply" })).toBeVisible();
    await transcript.evaluate((element) => {
      element.scrollTop = 0;
    });
    await page.screenshot({ path: info.outputPath("top-1280-light.png") });

    await transcript.getByRole("button", { name: "Open screenshot.png" }).click();
    const viewer = page.getByRole("dialog", { name: "screenshot.png" });
    await expect(viewer.getByRole("img", { name: "screenshot.png" })).toBeVisible();
    await page.screenshot({ path: info.outputPath("viewer-1280-light.png") });
    await page.keyboard.press("Escape");
    await expect(viewer).toBeHidden();

    for (const [width, scheme] of [
      [1280, "light"],
      [1280, "dark"],
      [390, "light"],
      [390, "dark"],
      [320, "light"],
    ] as const) {
      await page.setViewportSize({ width, height: width > 400 ? 900 : 844 });
      await useScheme(page, scheme);
      await page.reload();
      if (width < 1024) {
        await expect(page.getByRole("button", { name: "Chats", exact: true })).toBeVisible();
        await expect(chatList(page)).toBeHidden();
        await page.getByRole("button", { name: "Chats", exact: true }).click();
        await chatButton(page, "Fix the login copy").click();
      } else {
        await chatButton(page, "Fix the login copy").click();
      }
      await expect(transcriptOf(page).getByRole("heading", { name: "Result" })).toBeVisible();
      await expect(composerOf(page)).toBeVisible();
      await expectNoSidewaysScroll(page);
      await page.screenshot({ path: info.outputPath(`history-${width}-${scheme}.png`) });
      if (width !== 320) {
        await transcriptOf(page).evaluate((element) => {
          element.scrollTop = 0;
        });
        await page.screenshot({ path: info.outputPath(`history-top-${width}-${scheme}.png`) });
      }
    }
  } finally {
    await fake.stop();
  }
});

test("a pasted screenshot uploads at once and the run streams into the chat", async ({
  page,
  request,
}, info: TestInfo) => {
  const fake = new FakeAgent();
  await fake.start();
  try {
    await installFakeAgent(page, fake);
    await adminPage(page, request);
    await page.setViewportSize({ width: 1280, height: 900 });
    await useScheme(page, "light");
    await page.goto("/agent");

    const composer = composerOf(page);
    await expect(composer).toBeVisible();
    let release: () => void = () => undefined;
    fake.uploadGate = new Promise<void>((resolve) => (release = resolve));
    await composer.focus();
    await composer.evaluate((element, base64) => {
      const bytes = Uint8Array.from(atob(base64), (character) => character.charCodeAt(0));
      const data = new DataTransfer();
      data.items.add(new File([bytes], "pasted.png", { type: "image/png" }));
      element.dispatchEvent(
        new ClipboardEvent("paste", { clipboardData: data, bubbles: true, cancelable: true }),
      );
    }, PICTURE.toString("base64"));

    await expect(page.getByRole("status", { name: "Uploading pasted.png" })).toBeVisible();
    await expect(page.getByRole("img", { name: "pasted.png" })).toBeVisible();
    await composer.fill("What is wrong with this screenshot?");
    await expect(page.getByRole("button", { name: "Send", exact: true })).toBeDisabled();
    release();
    await expect(page.getByRole("status", { name: "Uploading pasted.png" })).toBeHidden();
    await expect(page.getByRole("button", { name: "Send", exact: true })).toBeEnabled();
    expect(fake.uploads).toHaveLength(1);
    expect(fake.uploads[0].filename).toBe("pasted.png");
    expect(fake.uploads[0].type).toBe("image/png");
    expect(fake.uploads[0].bytes.equals(PICTURE)).toBe(true);
    await page.screenshot({ path: info.outputPath("tray-1280-light.png") });
    await page.setViewportSize({ width: 390, height: 844 });
    await page.screenshot({ path: info.outputPath("tray-390-light.png") });
    await page.setViewportSize({ width: 1280, height: 900 });

    await page.locator("input[type='file']").setInputFiles({
      name: "picked.png",
      mimeType: "image/png",
      buffer: PICTURE,
    });
    await expect(page.getByRole("img", { name: "picked.png" })).toBeVisible();
    await expect.poll(() => fake.uploads.length).toBe(2);
    expect(fake.uploads[1].filename).toBe("picked.png");
    await page.getByRole("button", { name: "Remove picked.png" }).click();
    await expect(page.getByRole("img", { name: "picked.png" })).toHaveCount(0);

    const runId = "run-live";
    const conversationId = "conv-new";
    const user = {
      id: "msg-live-1",
      conversation_id: conversationId,
      role: "user",
      content: "What is wrong with this screenshot?",
      run_id: runId,
      created_at: iso(0),
      attachments: [attachment("att-upload-1", "pasted.png")],
    };
    fake.onMessage = () => {
      const started = run(runId, "running", {
        conversation_id: conversationId,
        started_at: iso(0),
      });
      fake.conversations.set(conversationId, {
        id: conversationId,
        engine: "assistant",
        title: "What is wrong with this screenshot?",
        created_at: iso(0),
        updated_at: iso(1),
        messages: [user],
        runs: [started],
        events: [
          event(runId, 1, "status", "Working with DeepSeek V4.1 Flash", { tools: ["repo_read"] }),
        ],
      });
      return { run: started };
    };
    await composer.press("Enter");

    await expect(page.getByText("What is wrong with this screenshot?").first()).toBeVisible();
    await expect(composer).toHaveValue("");
    await fake.connected(runId);
    await expect(page.getByRole("button", { name: "Stop", exact: true })).toBeEnabled();
    await expect(page.getByRole("button", { name: "Send", exact: true })).toHaveCount(0);

    fake.push(runId, "live", { text: "Looking at the **screenshot** now" });
    await expect(transcriptOf(page).getByText("Looking at the")).toBeVisible();
    await expect(transcriptOf(page).locator("strong", { hasText: "screenshot" })).toBeVisible();
    await expect(
      transcriptOf(page).getByRole("status").filter({ hasText: "Working for" }),
    ).toBeVisible();

    const first = event(runId, 2, "text", "I can see a title bar and one panel.");
    fake.push(runId, "event", first);
    fake.push(runId, "live", { text: "" });
    await expect(
      transcriptOf(page).getByText("I can see a title bar and one panel."),
    ).toBeVisible();

    fake.push(
      runId,
      "event",
      toolCall(runId, 3, "1-0", "repo_read", "Read apps/web/src/routes/agent/+page.svelte", {
        path: "apps/web/src/routes/agent/+page.svelte",
      }),
    );
    const step = transcriptOf(page).getByRole("button", {
      name: /Read apps\/web\/src\/routes\/agent/,
    });
    await expect(step).toContainText("Running");
    fake.push(
      runId,
      "event",
      toolResult(
        runId,
        4,
        "1-0",
        "repo_read",
        "apps/web/src/routes/agent/+page.svelte, lines 1-40 of 300\n1\t<script>",
      ),
    );
    await expect(step).not.toContainText("Running");
    await expect(step).toContainText("0.4s");
    await page.screenshot({ path: info.outputPath("streaming-1280-light.png") });

    const answer = "The panel text is cropped. **Fixed** in the next commit.";
    fake.push(runId, "event", event(runId, 5, "text", answer));
    fake.conversations.set(conversationId, {
      id: conversationId,
      engine: "assistant",
      title: "What is wrong with this screenshot?",
      created_at: iso(0),
      updated_at: iso(20),
      messages: [
        user,
        {
          id: "msg-live-2",
          conversation_id: conversationId,
          role: "assistant",
          content: answer,
          run_id: runId,
          created_at: iso(19),
        },
      ],
      runs: [
        run(runId, "succeeded", {
          conversation_id: conversationId,
          finished_at: iso(19),
          input_tokens: 4200,
          output_tokens: 310,
          summary: answer,
        }),
      ],
      events: [
        event(runId, 1, "status", "Working with DeepSeek V4.1 Flash", { tools: ["repo_read"] }),
        first,
        toolCall(runId, 3, "1-0", "repo_read", "Read apps/web/src/routes/agent/+page.svelte", {
          path: "apps/web/src/routes/agent/+page.svelte",
        }),
        toolResult(
          runId,
          4,
          "1-0",
          "repo_read",
          "apps/web/src/routes/agent/+page.svelte, lines 1-40 of 300\n1\t<script>",
        ),
        event(runId, 5, "text", answer),
      ],
    });
    fake.push(
      runId,
      "run",
      run(runId, "succeeded", { conversation_id: conversationId, finished_at: iso(19) }),
    );
    fake.push(runId, "done", { status: "succeeded" });

    await expect(page.getByRole("button", { name: "Send", exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: "Stop", exact: true })).toHaveCount(0);
    await expect(transcriptOf(page).getByText("The panel text is cropped.")).toHaveCount(1);
    await expect(transcriptOf(page).locator("strong", { hasText: "Fixed" })).toBeVisible();
    await expect(transcriptOf(page).getByRole("button", { name: "Open pasted.png" })).toBeVisible();
    await expect(
      transcriptOf(page).getByRole("status").filter({ hasText: "Working for" }),
    ).toHaveCount(0);
    await expect(chatButton(page, "What is wrong with this screenshot?")).toBeVisible();

    const messages = fake.calls("POST", /messages$/);
    expect(messages).toHaveLength(1);
    expect(messages[0].body).toEqual({
      prompt: "What is wrong with this screenshot?",
      attachment_ids: ["att-upload-1"],
      model: "deepseek-v4.1-flash",
      effort: "auto",
      access: "full",
    });
    expect(fake.calls("POST", /^\/conversations$/)[0].body).toEqual({ engine: "assistant" });
    await expectNoSidewaysScroll(page);
    await page.screenshot({ path: info.outputPath("finished-1280-light.png") });
  } finally {
    await fake.stop();
  }
});

test("a running task can be stopped, and a chat can be deleted after a confirmation", async ({
  page,
  request,
}, info: TestInfo) => {
  const fake = new FakeAgent();
  await fake.start();
  try {
    const running = storedConversation();
    const runId = "run-history";
    running.runs = [
      run(runId, "running", { conversation_id: "conv-history", started_at: iso(-20) }),
    ];
    running.messages = [(running.messages as Json[])[0]];
    running.events = [event(runId, 1, "text", "Starting the investigation.")];
    fake.conversations.set("conv-history", running);
    await installFakeAgent(page, fake);
    await page.route(/\/api\/v1\/admin\/agent\/runs\/run-history\/cancel$/, async (route) => {
      fake.conversations.set("conv-history", {
        ...running,
        runs: [run(runId, "canceled", { conversation_id: "conv-history", finished_at: iso(0) })],
      });
      await route.fulfill({ json: { run: run(runId, "canceled", { finished_at: iso(0) }) } });
    });
    await adminPage(page, request);
    await page.setViewportSize({ width: 1280, height: 900 });
    await page.goto("/agent");

    await chatButton(page, "Fix the login copy").click();
    await fake.connected(runId);
    await expect(transcriptOf(page).getByText("Starting the investigation.")).toBeVisible();
    const stop = page.getByRole("button", { name: "Stop", exact: true });
    await expect(stop).toBeEnabled();
    await composerOf(page).press("Escape");
    await expect(page.getByRole("button", { name: "Send", exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: "Stop", exact: true })).toHaveCount(0);

    await page.getByRole("button", { name: "Delete chat Fix the login copy" }).click();
    const dialog = page.getByRole("dialog", { name: "Delete this chat?" });
    await expect(dialog).toBeVisible();
    await page.screenshot({ path: info.outputPath("delete-confirm-1280-light.png") });
    expect(fake.calls("DELETE", /conversations/)).toHaveLength(0);
    await dialog.getByRole("button", { name: "Delete", exact: true }).click();
    await expect(page.getByText("What should the agent work on?")).toBeVisible();
    await expect(chatList(page).getByText("No chats yet.")).toBeVisible();
    expect(fake.calls("DELETE", /conversations\/conv-history/)).toHaveLength(1);
  } finally {
    await fake.stop();
  }
});
