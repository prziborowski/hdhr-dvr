# HDHomeRun DVR - AGENTS.md

## Repo layout

| Path | Purpose |
|------|---------|
| `cmd/app/app.go` | Main DVR app — single large file (~1800 LOC). All DB helpers, HTTP handlers, ffmpeg recording logic |
| `cmd/guide/guide.go` | CLI: fetches EPG from TitanTV and pushes it to the app over HTTP (POST /api/guide); no shared `guide.json` |
| `cmd/auto-record/main.go` | CLI: matches guide programs against keywords, schedules recordings via API |
| `update_sizes.go` | Standalone script: scans filesystem and sets file_size in DB |
| `pkg/config/config.go` | Config struct + LoadConfig() from `config.json` |
| `pkg/types/types.go` | Domain types (Channel, Recording, Program, TitanTV responses) |

## Build & run

```bash
bin/build.sh        # Builds all binaries to bin/{app,guide,auto-record}
```

Verify with:
```bash
bin/test.sh              # Run all pkg unit tests (required)
golangci-lint fmt ./...  # Should not error
golangci-lint run ./...  # Should be clean
```

Tests are mandatory. Every code change must include relevant unit tests, and `bin/test.sh` must pass before considering a change complete. Add tests in `pkg/<package>/<package>_test.go` for each package.

No Makefile. The app expects `config.json` in the working directory.

## config.json (single source of truth)

Fields: `timezone`, `lineUpID`, `days`, `storageDir`, `userId`

## Architecture notes

- **Single-file main**: `cmd/app/app.go` contains everything — HTTP server, DB operations, recording logic.
- **DB**: SQLite at `./recordings.db`. Connection pool: MaxOpenConns=10, MaxIdleConns=5.
- **No context timeout wrapping in db helpers**: `dbQueryContext`, `dbExecContext`, and `dbQueryRowContext` are thin passthroughs to `db.QueryContext/ExecContext/QueryRowContext`. Callers manage their own timeouts — do NOT add `context.WithTimeout` inside these helpers or you'll get "context canceled" errors.
- **Recording lifecycle**: pending → recording → completed/failed. Status transitions involve file existence checks on disk.
- **Startup sequence** (in `main()`): init DB → load config → fetch tuner count → create tables → load channels → load guide from DB (loadGuideFromDB) → load recordings → cleanup old → start scheduler goroutine.
- **Guide ingest**: `POST /api/guide` (with `PUT /api/guide` as an alias) replaces the in-memory and stored guide with a full EPG snapshot; guide data is persisted in the app's SQLite DB (`guide_channels`/`guide_programs`/`guide_meta` tables) instead of a watched `guide.json` file.

## Key patterns & gotchas

- `cleanupOldRecordings` uses the collection-then-update pattern: collect pending/recording row IDs and computed end times, close the cursor, then UPDATE separately to avoid SQLite "database is locked" errors.
- The scheduler goroutine in `startRecordingScheduler` queries pending recordings every minute — it uses raw `db.QueryContext` (not the wrapper) directly with its own context.
- Recording deletion must check `dbQueryRowContext` for existence first — foreign key constraints may apply.
- TitanTV API calls require a User-Agent header and respect rate limiting (5s sleep between schedule blocks).
