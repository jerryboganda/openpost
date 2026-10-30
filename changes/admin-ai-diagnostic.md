### Added

- Added an administrator-only AI provider diagnostic that sends one explicit model request and reports the provider, model, response validity, latency, and a stable failure category. Routine liveness and readiness checks never call a model, and provider response bodies or credentials are never returned.
