### Added

- Rebuilt the administrator Agent as a chat. Replies stream as they are written, each tool call is a step you can open, and the transcript follows new output only while you stay at the bottom. Conversations can be opened, resumed after a reload, and deleted. Esc stops a running task.
- The built-in OpenPost Agent can now act, not only advise. It reads and edits code on a branch, opens and merges pull requests, starts and reads GitHub Actions runs, inspects production status and logs, runs read-only database queries with secret columns masked, and calls any OpenPost API operation as the signed-in administrator. It never writes to the default branch, never commits credentials, and refuses to merge while checks fail unless told to. Read-only access removes every tool that changes something.
- Attach screenshots to a message by pasting, dropping, or picking images (PNG, JPEG, GIF, or WebP, up to 5 per message). The browser compresses large images before upload, the server checks the real file type and size, and a model that cannot read images is swapped for one that can.
- Choose the model, reasoning effort, and access level for each message. The chosen model and the token usage of each task are recorded with the run.
- Added `OPENPOST_AGENT_ALLOW_PIPELINE_EDITS`. By default the agent cannot change files under `.github/` or `deploy/`, because they run with the repository's secrets and text the agent reads, such as a stranger's pull request, could try to steer it there. Set it to `true` to let the agent edit workflows.

### Changed

- The built-in agent no longer answers from a text-only snapshot. It follows a loop of reasoning and tool calls for up to 60 steps, shows each result as it arrives, and reports tool failures instead of guessing.
- The agent's generic GitHub tool no longer writes files, git objects, webhooks, deploy keys, collaborators, environments, or direct merges into the default branch. File changes go through the repo tools, which check the path and the content. The release workflow's deploy-only mode accepts only images from this repository's registry.

### Fixed

- Reasoning that some models write into their answer between `<think>` tags is hidden from the transcript and from stored answers, while the model still receives its own words back.
