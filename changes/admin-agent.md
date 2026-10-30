### Added

- Added an administrator-only Agent workspace with per-conversation engine selection, durable history, streamed progress, cancellation, and an inspectable action log.
- Added a built-in OpenCode assistant that answers immediately with the instance's configured AI provider, so the chat works without GitHub Actions or a coding-engine credential. It includes a live instance snapshot and states when a request needs a coding engine.
- Coding-agent runs execute in GitHub Actions. The server dispatches by workflow, fetches the private task context over a signed request, and accepts signed progress callbacks, so prompts never appear in public workflow inputs and the VPS never runs coding work.
- Missing engine credentials, dispatch failures, interrupted runs, and engine failures are recorded truthfully. A server restart marks in-flight runs as interrupted with an unknown outcome instead of guessing.
