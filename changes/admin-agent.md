### Added

- Added an administrator-only Agent workspace with per-conversation engine selection, durable history, streamed progress, cancellation, and an inspectable action log.
- Coding-agent runs execute in GitHub Actions. The server dispatches by workflow, fetches the private task context over a signed request, and accepts signed progress callbacks, so prompts never appear in public workflow inputs and the VPS never runs coding work.
- Missing engine credentials, dispatch failures, interrupted runs, and engine failures are recorded truthfully. A server restart marks in-flight runs as interrupted with an unknown outcome instead of guessing.
