# Administrator Agent

The Agent is an administrator-only chat at `/agent`. Its built-in engine (`assistant`, shown as "OpenPost Agent") runs a streaming tool-calling loop in the server. The coding engines (OpenCode, Codex, Claude Code) still run on GitHub Actions, as before.

## Where it lives

```text
apps/server/internal/ai/chat.go                       streaming, tool-calling, image-capable chat client
apps/server/internal/services/adminagent/
  agent.go          run loop (60 steps), system prompt, history, vision hand-off
  service.go        conversations, runs, events, Actions engines, deleting
  models.go         the OpenCode Go model catalogue and which models read images
  tools_repo.go     repo_tree, repo_read, repo_search, repo_edit, repo_write, repo_delete
  tools_ci.go       pr_*, ci_*, github_api
  tools_ops.go      prod_status, prod_logs, db_query
  tools_api.go      api_search, api_call (through the app's own router)
  github.go         GitHub client and the write guards
  toolbox.go        tool plumbing and redaction
  reasoning.go      hides <think> blocks that a model writes into its answer
  attachments.go    uploaded images (4 MB, 5 per message, sniffed, 64 MP bound)
apps/server/internal/api/handlers/admin_agent.go      Huma routes, the SSE stream, and the upload
apps/web/src/lib/agent/                               controller, transcript model, markdown, diff, image policy
apps/web/src/lib/components/agent/                    chat components
apps/web/src/routes/agent/+page.svelte
```

A run is durable. The server owns it, so closing the tab loses nothing: the page reads the stored conversation and follows the event stream from the last event it saw.

## Guard rails

The agent holds a broad GitHub token and calls the API as the administrator, so every limit lives in code, not in the prompt. Keep each one when adding a tool, and cover it with a test that shows the refused request never reaches GitHub.

- Writes never go to the default branch, `.env` files, key files, or credential-shaped text.
- `.github/` and `deploy/` run with the repository's secrets. They are refused unless `OPENPOST_AGENT_ALLOW_PIPELINE_EDITS=true`.
- `github_api` refuses file and git-object writes (the repo tools check them), webhooks, deploy keys, collaborators, environments, rulesets, direct merges into the default branch, and encoded or traversal paths. It compares whole path segments, so a branch named after a section is fine.
- `db_query` is one `SELECT`, in a read-only transaction on PostgreSQL, with a statement timeout. Secret columns are masked by name, and password hashes are redacted by shape.
- `api_call` runs through the router with a per-run administrator session that is revoked afterwards. It refuses the agent's own endpoints, non-GET `/auth/*`, and non-GET backup paths.
- `pr_merge` refuses while checks fail unless told to.
- Read-only access never offers a tool that changes something.
- Text in tool results (files, logs, pull requests, database rows) is data. The system prompt says so, but it is only a backstop for the limits above.

## Verifying a change

All three run on GitHub Actions. Nothing heavy runs on the production server.

- **Fork Quick Check** starts on a push to `feat/**`, `fix/**`, or `agent/**`. It runs the Go build, vet, lint and focused tests (repeated under the race detector, because runs work in the background), the web lint, type check and unit tests, and the repository policy checks.
- **Fork Agent UI Check** starts on those pushes when the Agent UI or its spec changes. It opens the built app in Chromium with the agent API replaced by fixtures, and keeps screenshots at 1280, 390 and 320 px in light and dark.
- **Fork Agent Live Check** starts when its own workflow file is edited, or by hand. It makes every catalogue model stream, chain tools, and read an image against the real OpenCode endpoint. It needs the `OPENCODE_API_KEY` secret.

A workflow that is not on the default branch cannot be dispatched by hand, so edit the workflow file on a branch to run it there.
