# TSQL (tamamiSQL)

Small but **real** relational SQL database server in Go, inspired by PostgreSQL.
Runs on Tamami Linux 43, installs like `postgresql` (dnf + systemd), accessed via
its own CLI `tsql` over a custom framed protocol.

Status: **v1 complete** (Fase 0–4) — see [design spec](docs/superpowers/specs/2026-09-14-tsql-design.md).

## What it does

**Implemented (Fase 0–4):**

- SQL: `CREATE TABLE`, `DROP TABLE [IF EXISTS]`, `INSERT ... VALUES` (multi-row),
  `UPDATE`, `DELETE`, `SELECT` with `WHERE`, `ORDER BY` (multi-key, ASC/DESC),
  `LIMIT`, and `NOW()`.
- `INNER JOIN` (chained, left-deep) with table aliases and qualified
  columns (`t.col`); ambiguous unqualified columns are rejected.
- `GROUP BY` + aggregates: `COUNT(*)`, `COUNT(col)`, `SUM`, `AVG`, `MIN`,
  `MAX` (NULLs skipped; empty input → single row with 0/NULL).
- `IS [NOT] NULL`.
- `PRIMARY KEY` (single int column, optional `AUTO_INCREMENT`),
  `NOT NULL`, `UNIQUE` — UNIQUE columns are backed by a secondary index
  (equality-lookup shortcut for `WHERE col = literal`, duplicate rejection
  with SQLSTATE `23505`, NULLs allowed multiple times).
- **Transactions:** `BEGIN` / `COMMIT` / `ROLLBACK` per connection with
  buffered writes — no dirty reads, read-your-own-writes, last-committer-wins.
  A failed statement or disconnect aborts the transaction. WAL is appended at
  COMMIT time (one fsync); PK and UNIQUE conflicts are re-checked against the
  committed state at commit. DDL is autocommit and rejected inside an
  explicit transaction (`25000`).
- **Foreign keys (RESTRICT):** `col INT REFERENCES parent [ (col) ]` (target
  defaults to the parent's primary key). NULL FK values allowed; violations on
  insert/update and on parent PK delete/PK-move return `23503`. Enforced
  against the transaction's merged view, so same-txn parent+child inserts work.
- **Multi-connection:** one goroutine per connection; concurrent clients with
  interleaved BEGIN/COMMIT cycles — no lost updates.
- **Packaging (Fase 4):** systemd unit (`tsqld.service`, dedicated `tsql`
  user, data in `/var/lib/tsql`, TCP `127.0.0.1:5433` + unix socket
  `/run/tsql/tsqld.sock`), dnf-installable RPM (`packaging/tsql.spec`,
  static CGO-less binaries, service auto-created + auto-started, data kept
  on uninstall), and a quick `install.sh` for /usr/local installs.
- Crash-safe persistence: WAL + snapshots, replay on startup (torn tails
  truncated; uncommitted txns dropped).
- Error codes follow PostgreSQL SQLSTATEs (42P01, 42P07, 42703, 22P02,
  23502, 23503, 23505, 25000, 25006, 42803, 42804, 42883, 22012, 21000, ...).
- Own framed protocol (`[4-byte length][JSON]`) + `tsql` CLI with
  psql-style aligned tables.

**Planned (v2, beyond the Fase 0–4 spec):** LEFT/OUTER JOIN, HAVING,
subqueries, MVCC, per-connection auth.

Full design: [spec](docs/superpowers/specs/2026-09-14-tsql-design.md).

## Quickstart (dev)

```bash
make build
./bin/tsqld -datadir /tmp/tsqldemo
./bin/tsql -c "CREATE TABLE users (id int PRIMARY KEY, name text)"
./bin/tsql -c "INSERT INTO users (name) VALUES ('alfa')"
./bin/tsql -c "SELECT * FROM users"
```

## Installation (Tamami Linux / RHEL-style)

**RPM:**

```bash
dnf install -y rpm-build
make dist-tarball && make rpm     # -> ~/rpmbuild/RPMS/x86_64/tsql-0.3.0-1.*.rpm
sudo dnf install ~/rpmbuild/RPMS/x86_64/tsql-0.3.0-1.*.rpm
systemctl status tsqld
```

The RPM creates the `tsql` system user and `/var/lib/tsql`, installs
`tsqld.service` (listens on `127.0.0.1:5433` + unix socket
`/run/tsql/tsqld.sock`) and starts it. The daemon checkpoints on SIGTERM and
auto-restarts after a crash; the data directory is kept on uninstall.

**Quick install (no rpm):**

```bash
sudo ./install.sh        # builds + installs to /usr/local/bin, unit + service
```

## Layout

```
cmd/tsqld        server daemon
cmd/tsql         CLI client
internal/...     parser, executor, storage (WAL+snapshots+txns), protocol
packaging/       systemd unit + RPM spec
install.sh       quick /usr/local install
docs/...         design spec
```

## Tests

```bash
go test ./...
./test/e2e.sh        # boots a real daemon, runs real queries
```
