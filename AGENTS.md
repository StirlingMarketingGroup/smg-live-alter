# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

`smg-live-alter` is a single-binary Go CLI for applying MySQL `ALTER TABLE` statements without downtime. It is an alternative to `pt-online-schema-change` (Percona) and `gh-ost` (GitHub) — see `README.md` for the rationale (mainly: trigger preservation, simplicity, correct handling of binary columns).

The tool is published as a Go module: `github.com/StirlingMarketingGroup/smg-live-alter/v2` (the `/v2` suffix matters — the module path includes it).

## Common commands

```sh
go build ./...                                # compile
go vet ./...                                  # static checks
go install github.com/StirlingMarketingGroup/smg-live-alter/v2@latest   # install from upstream
go run . 'user:pass@(host)/dbname'            # run from source against a DSN
go run . localhost                            # run using a named connection from connections.yaml
```

There are no tests in the repo; do not assume `go test` covers behavior changes.

The connections file lives at `os.UserConfigDir()/smgla/connections.yaml` (e.g. `~/Library/Application Support/smgla/connections.yaml` on macOS, `~/.config/smgla/connections.yaml` on Linux). See `connections-example.yaml`. Note the README claims `~/.config/smg-live-alter/...` but the code in `main.go:24` uses `smgla` — the code is authoritative.

The alter statement itself is collected interactively by opening `nano` on a temp file (`prompt.go`), not via flags or stdin. There is no non-interactive mode.

## How the live alter works (read this before changing core logic)

The flow in `main.go` is sequential and the ordering matters. Broadly:

1. **Build the temp table.** Run `SHOW CREATE TABLE`, strip the `CONSTRAINT` block (FK names are globally unique in MySQL, so you can't recreate them under a different table name without conflict — they are re-added at the very end). Create `<table>_smgla_` from the modified DDL, then apply the user's alter to it.
2. **Diff columns.** `getTableColumns` (`columns.go`) pulls column metadata from `information_schema.columns` plus PK info from `SHOW INDEX`. The intersection of old↔new columns drives both the select-list and the trigger bodies. Generated columns (non-empty `GENERATION_EXPRESSION`) are filtered out everywhere — MySQL refuses inserts into them. `parseChangeColumnRegex` in `regexp.go` extracts `CHANGE COLUMN old_name new_name` mappings so renames are tracked across old→new column names.
3. **Install three triggers** on the source table (`*_after_insert_smgla_`, `*_after_update_smgla_`, `*_after_delete_smgla_`) that mirror writes into the temp table. The insert uses `INSERT IGNORE` so it tolerates a row that the bulk copy is about to insert anyway. The update trigger does *both* an insert (in case the row hasn't been copied yet) and an update.
4. **Bulk-copy rows in PK-ordered chunks.** Driven by reflection in `main.go` around lines 312–375: `tableRowStruct` (`struct.go`) builds a `dynamic-struct` whose fields are `F0`, `F1`, ... typed per MySQL data type, with `mysql:"<column>,omitempty"` tags. A `chan` of that struct type is created via `reflect.MakeChan`, and `cool-mysql`'s `Insert` consumes it. The producer goroutine pages through the source with `WHERE (pk1,pk2,...) > (prev1,prev2,...) ORDER BY pks LIMIT @r`, capturing the last row's PK values inside a reflective callback (`destFunc`) to drive the next page.
5. **Adaptive chunk sizing.** `SetAfterChunkExec` measures wall time per insert chunk against a 500 ms target. Over-target → drop `MaxInsertSize` proportionally immediately. Under-target → grow by 10% of the gap (slow start). The increase is capped against `originalMaxInsertSize` — see the condition at `main.go:414`. This is a deliberate feedback loop, not a hot path to micro-optimize.
6. **Drop / swap / restore.** After a `do the drop/swap?` y/n confirmation: disable foreign-key checks for the connection, snapshot the original triggers via `SHOW TRIGGERS` + `SHOW CREATE TRIGGER` (filtering out the three `_smgla_` triggers), drop the original table, sleep 1s, re-add constraints by converting the saved `,\n  CONSTRAINT ...` block into `ALTER TABLE ... ADD CONSTRAINT ...` lines, recreate the original triggers (rewritten to point at the temp name via `renameTriggerTable` in `triggers.go`), then `ALTER TABLE ... RENAME` the temp into place.

The drop happens *before* the rename specifically to avoid a window where both tables exist with conflicting state — the README spells this out. There is intentionally a brief window where the table name does not resolve; callers must retry. Atomic `RENAME ... TO` is *not* used because it would also retarget FKs in other tables to the renamed table, and we want those FKs to land on the new schema.

The drop+swap was previously wrapped in a transaction; commit `d383cad` removed that on purpose ("dont use tx for the drop and swap"). Don't reintroduce a transaction around steps 6+.

### PK changes

If old and new PK column counts differ, `main.go:242` checks the intersection: as long as the old PKs are a subset of new (or vice versa), it picks the shared set and proceeds. If the PK columns *and* the count both diverge, the tool panics. This was added in `7d2af2d`.

### Type → Go type mapping

`struct.go` switches on `DATA_TYPE` (not `COLUMN_TYPE`, since the latter includes lengths/`unsigned`). All fields are pointers so NULLs scan cleanly. Two non-obvious choices:

- `decimal` and `double` → `*mysql.Raw`. `cool-mysql` will inline the value verbatim — safe because MySQL-formatted decimals can't contain breaking characters, and avoids float→string round-trip precision loss.
- `json` → `*json.RawMessage`. `cool-mysql` recognizes this type and emits the right charset wrapper; using `[]byte` would lose UTF-8 charset info MySQL needs for JSON columns.

Adding a new MySQL type means: add the case to `struct.go`'s switch *and* make sure it round-trips through `cool-mysql`'s insert path.

## Code map

- `main.go` — full orchestration (steps above). Most logic still lives here; resist the urge to split it just for tidiness.
- `columns.go` — `information_schema` queries; PK detection. Also handles older MySQL versions that lack `GENERATION_EXPRESSION` (probes `information_schema` for the column itself before selecting it).
- `struct.go` — dynamic row struct construction. PK column indices are returned alongside the struct so the producer goroutine knows which fields to read for the `prevIDs` cursor.
- `regexp.go` — two regexes: parse the user's alter into `(schema, table, alter-body)`, and find `CHANGE COLUMN` renames.
- `triggers.go` — rewrites `CREATE TRIGGER ... ON \`table\` ...` to point at a different table by replacing the last backtick-quoted identifier on the first line.
- `prompt.go` — `nano`-based alter input, `Y/n` confirmation reader.
- `connections.go` — YAML config loader and DSN builder (uses `go-sql-driver/mysql`'s `NewConfig` then appends URL-escaped extra params).

## Conventions

- Error handling is `panic`-on-error throughout `main.go`. This is intentional — there is no recovery path mid-alter, and a panic is the clearest signal that the in-progress migration is in a partial state. Don't convert these to silent error returns.
- SQL is built with concatenation and backtick-quoted identifiers. Table names come from the user's alter statement (parsed by regex), not from arbitrary input — but be careful adding new code paths that would interpolate untrusted strings into DDL.
- `cool-mysql` query placeholders use `@@name` (compile-time interpolated, often as `mysql.Raw`) and `@name` (parameter-bound). The bulk select uses `@@`-style for column lists / table name / where clause and a single bound `@@limit`.
