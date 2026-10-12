# Agent instructions

## Working agreement

- Inspect the current branch, worktree, relevant code and existing checks before editing. Preserve unrelated local changes.
- Keep authorized changes on `main`. Do not create branches, commit, tag, push or publish unless the user requests it.
- Analyze the underlying requirement before choosing an implementation. If the goal is unclear, discuss it; if a simpler approach meets the goal, explain it.
- Use Go tools for this Go module. If supporting JavaScript/TypeScript or Python scripts are needed, use `bun`/`bunx` and `uv` respectively.

## Module boundaries

- The repository root contains only the executable entry point, linker-injected metadata and C ABI adapter.
- `internal/plugin` owns host registration, lifecycle, request interception, usage ingestion and management routes. It coordinates the feature packages.
- `internal/config` parses YAML configuration and resolves the database path.
- `internal/protocol` owns host request/response types and JSON envelopes.
- `internal/model` owns shared usage and safety data types and normalization.
- `internal/storage` owns SQLite connections, usage/safety persistence, migrations and coordination of pricing rebuilds.
- `internal/statistics` owns pricing calculations, aggregates and time buckets.
- `internal/notifications` owns notification settings, subscriptions, period settlement, outbox, scheduling and SMTP delivery.
- `internal/safety` owns API key extraction, policy failure recognition and limiter state transitions.
- `internal/testfixture` and `internal/testutil` contain shared test fixtures and assertions; production code must not import them.

Put code in the package that owns its responsibility. Feature packages must not import `internal/plugin`. Keep dependencies acyclic and export only operations and types needed across package boundaries. Keep white-box tests beside their implementation and integration tests beside the coordinator they exercise. Put inspection helpers needed by external tests in `export_test.go` instead of widening the production API.

## Behavior to preserve

- Preserve C ABI symbols, layout, buffer ownership and shutdown behavior. Keep `main.pluginVersion`, `main.pluginAuthor` and `main.pluginRepository` available to the release scripts.
- Preserve management paths, host schema negotiation, JSON field names and envelope bytes. The host mounts management routes under `/v0/management`.
- The independent plugin ID is `jacko-usage-statistics`; management routes and release artifact names use this ID. The historical default data directory remains `usage-statistics`, independently of the plugin identity. Keep the registry metadata aligned with host registration, and point its homepage to the Jacko-John management frontend.
- Preserve existing SQLite data and migrations. Safety history protects linked usage records from deletion.
- Use the store's single writer connection to serialize SQL commits and in-memory statistics updates. Update memory only after persistence succeeds.
- Preserve lock ordering and the WAL snapshot plus pending-delta protocol during pricing rebuilds. Settle due notification periods using the old prices before installing new prices.
- Stop and join the notification worker before closing SQLite. Never hold the writer connection during SMTP delivery.
- Keep token accounting, pricing, half-open query intervals, time-zone/DST behavior, first notification period coverage and bounded retries unchanged during structural refactors.

## Verification

Run the checks appropriate to the change, including all of these for a module refactor:

```sh
go test ./...
go test -race ./...
go vet ./...
CGO_ENABLED=1 go build -buildmode=c-shared -o /tmp/jacko-usage-statistics.so .
```

If the environment restricts the default compiler cache, set `GOCACHE` to a writable temporary directory. SMTP tests use local TLS servers and need permission to bind localhost sockets; do not skip them silently. Validate `build.sh` and `package-release.sh` after entry point or metadata changes. Keep generated libraries, archives and scratch scripts out of the worktree. Report checks and any verification limits.
