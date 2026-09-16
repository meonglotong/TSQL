# TSQL — Design Spec

Date: 2026-09-14
Status: approved (verbal, chat) — "oke gass"

## 1. Overview

TSQL (tamamiSQL) is a small but **real** relational SQL database server written in Go,
inspired by PostgreSQL's interface and semantics (pragmatic subset). It runs on
Tamami Linux 43 (Fedora 43-based), is installed like `postgresql` (dnf + systemd),
and is accessed through its own CLI, `tsql`, over a custom framed TCP protocol.

## 2. Goals / Non-Goals (v1)

Goals:
- Real daemon (`tsqld`) + CLI client (`tsql`), installable via dnf, managed by systemd.
- SQL subset: `CREATE TABLE`, `DROP TABLE`, `INSERT`, `UPDATE`, `DELETE`,
  `SELECT` with `WHERE`, `JOIN` (inner), `GROUP BY` + aggregates
  (`COUNT/SUM/AVG/MIN/MAX`), `ORDER BY`, `LIMIT`, secondary indexes,
  foreign keys, single-column primary keys (optionally auto-increment).
- Types: `int`, `bigint`, `float`, `text`, `bool`, `timestamp`.
- Crash-safe persistence: WAL + periodic snapshots, replay on startup.
- Durability via WAL; writes serialized (single writer), reads concurrent.
- Transactions: `BEGIN / COMMIT / ROLLBACK`, buffered-write model.
- Multi-client: many concurrent connections.

Non-goals (v1):
- PostgreSQL wire-protocol compatibility (own protocol; `psql` cannot connect).
- Views, CTEs, subqueries, window functions, triggers, stored procedures.
- Replication, multi-node, fine-grained MVCC / row-level locking.
- Authentication in the protocol (bind control at OS level, like `trust` mode).

## 3. Components

- `cmd/tsqld` — server daemon.
  Flags: `-datadir` (default `/var/lib/tsql`), `-listen` (default `127.0.0.1:5433`),
  `-unixsocket` (default `/run/tsql/tsql.sock`, empty to disable).
- `cmd/tsql` — CLI client. `-c "SQL"` one-shot; otherwise interactive REPL
  (`>` prompt, `\q` quit, `\dt` list tables).
- `internal/types` — value types, comparisons, formatting.
- `internal/parser` — hand-written lexer + recursive-descent parser → AST.
  No external parser library (control over the PG-like subset, small dependency tree).
- `internal/planner` — validates against catalog; chooses full scan vs index lookup.
- `internal/executor` — runs the plan, produces rows / affected counts / errors.
- `internal/schema` — catalog: databases (single default DB `tsql` for v1),
  tables, columns, indexes, FKs; persisted to `meta.json`.
- `internal/storage` — `wal.go` (append-only log, per-record CRC),
  `snapshot.go` (gob-encoded table data + schema), `engine.go`
  (open/recover, single-writer coordination), `table.go` (in-memory rows + RWMutex).
- `internal/txn` — per-connection transaction context (buffered writes).
- `internal/protocol` — framing `[4-byte big-endian length][JSON payload]`,
  message types: `query`, `result`, `ok`, `error`, `ready`.
- `internal/server` — accept loop; one goroutine per connection; dispatch to executor.

## 4. Data Flow

### Read
client → frame → parse → plan (catalog check, index pick) → execute under
per-table read-lock → `result` frame.

### Write (inside a transaction)
client → parse → plan → executor buffers row mutations in the connection's txn
context (WAL records appended, **uncommitted**, tagged with txn id) → on
`COMMIT`: acquire write lock, append commit record (fsync), apply buffer to
tables, release, send `ready{in_txn:false}`.
On `ROLLBACK`: discard buffer; uncommitted WAL records are simply never
committed — recovery replays only committed txns.

### Isolation
- Readers see only committed state (no dirty reads).
- A txn reads its own buffered writes (read-your-own-writes).
- Conflicting writes: last committer wins (no MVCC in v1 — documented limit).

## 5. Storage Layout

```
<datadir>/
  tsql/
    meta.json            # catalog (schema, indexes, FKs)
    snapshot-<seq>.bin   # gob: schema + full table data
    manifest.json        # { "latest_snapshot": <seq> }
    wal.log              # append-only binary records
```

WAL record format: `[type:1B][txn_id:8B][payload...][crc32:4B]`.
Types: `Put`, `Delete`, `Commit`, `Begin`. Crash-safety = fsync after commit record.

Recovery: load latest snapshot (per manifest) → replay WAL records after
snapshot seq, applying only records of committed txns → open for service.

## 6. Error Handling

- SQLSTATE-like codes (`42P01` undefined table, `23505` uniqueness, `23503` FK,
  `42601` syntax, `58030` internal). `error` frame carries `code` + `message`.
- CLI prints `ERROR: <message>` and keeps the REPL alive; `-c` mode exits 1.
- WAL record CRC failure → replay stops at first bad record, logs loudly,
  keeps the last valid state (truncates the torn tail).

## 7. Testing

- `go test ./...` per package: parser (golden AST cases), storage
  (WAL replay + simulated crash by truncating the log), txn (commit/rollback
  isolation), executor (query semantics, index vs scan equivalence).
- E2E shell script (`test/e2e.sh`): start `tsqld` on a temp datadir + random
  port, run `tsql -c` statements, assert stdout, kill daemon.
- Full suite must pass **on the VM (Tamami Linux 43)** before push.

## 8. Packaging (Fase 4)

- `packaging/systemd/tsqld.service` — `User=tsql`, `RuntimeDirectory=tsql`,
  datadir `/var/lib/tsql` (created + owned by `tsql` in %post).
- `packaging/tsql.spec` — plain RPM spec, `%build: go build` (CGO_ENABLED=0).
- `install.sh` — quick local install (binary to /usr/local, unit installed, daemon-reload).
- dnf-installable path: `rpmbuild` on the VM → `dnf install ./tsql-*.rpm`.

## 9. Phases

| Fase | Deliverable | Exit criteria |
|---|---|---|
| 0 | types + storage (WAL, snapshot, engine, recovery) | unit tests green on VM, crash-replay test passes |
| 1 | parser + executor (basic SQL) + `tsqld` + `tsql` CLI | E2E: create/insert/select/update/delete on VM |
| 2 | JOIN + GROUP BY/agg + secondary indexes | index chosen & verified vs full scan |
| 3 | transactions + FK + multi-connection | concurrent test: N clients, no lost updates |
| 4 | packaging + systemd + rpm + install in Tamami | `dnf install` + `systemctl` + E2E on VM; push to GitHub |

## 10. Decisions Locked

1. Language: **Go** (single static binary, native concurrency, trivial rpm).
2. Protocol: **own framed JSON protocol** + own CLI (no PG wire compat in v1).
3. Storage: **WAL + lock-based** (single writer, many readers; crash-safe, no MVCC).
4. SQL scope: **full-ish** (JOIN, agg, index, FK, txn, concurrent clients).
5. Target: build & test **on the cuki-dev VM** (Tamami Linux 43), the real target.
6. Repo: `github.com/meonglotong/TSQL`, branch `main`.

## 11. Open Defaults (object within 24h, otherwise as-is)

- Default listen `127.0.0.1:5433` + unix socket; `-listen 0.0.0.0:5433` for LAN use.
- v1: single database named `tsql`; no per-connection auth.
- Primary key: single `int`/`bigint` column, `NOT NULL`, optional `AUTO_INCREMENT`.
