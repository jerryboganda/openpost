### Added

- The Agent's reasoning picker now offers each model the levels it really has, including Off, Extra high, and Max where a model supports them, instead of one fixed Low, Medium, and High. DeepSeek V4.1 Flash offers Off, Low, High, and Max, GPT-6 Luna offers Off through Max, and a model with no level to choose says so. OpenRouter models use the levels OpenRouter publishes, and OpenAI models use OpenAI's documented levels.
- Added `OPENPOST_AGENT_REASONING_EFFORTS` for endpoints whose model accepts levels OpenPost cannot detect.

### Changed

- Higher reasoning efforts get more room. The output budget grows to 64,000 tokens at Max, and a stream can stay silent for up to 12 minutes before it is cut off, so a model that thinks for a long time is no longer stopped.
- The Agent keeps the level you chose and works out what to send from the selected model. Switching to a model with fewer levels steps down, and switching back restores your choice.

### Fixed

- A reasoning level chosen for a model that does not reason, such as `gpt-4o-mini`, is no longer sent for the provider to reject.
