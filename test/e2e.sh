#!/usr/bin/env bash
# E2E test: builds real binaries, boots a real tsqld, runs real tsql
# statements against it, and verifies crash recovery.
set -euo pipefail
cd "$(dirname "$0")/.."

WORK=$(mktemp -d)
DAEMON_PID=""
cleanup() {
  if [ -n "$DAEMON_PID" ]; then
    kill "$DAEMON_PID" 2>/dev/null || true
    wait "$DAEMON_PID" 2>/dev/null || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

PORT=$(( 20000 + RANDOM % 20000 ))
go build -o "$WORK/tsqld" ./cmd/tsqld
go build -o "$WORK/tsql" ./cmd/tsql

TSQ="$WORK/tsql -h 127.0.0.1 -p $PORT"

start_daemon() {
  "$WORK/tsqld" -datadir "$WORK/data" -listen "127.0.0.1:$PORT" >"$1" 2>&1 &
  DAEMON_PID=$!
  for _ in $(seq 1 50); do
    if (exec 3<>"/dev/tcp/127.0.0.1/$PORT") 2>/dev/null; then
      exec 3>&- 3<&- || true
      return 0
    fi
    sleep 0.2
  done
  echo "FAIL: daemon did not come up on port $PORT" >&2
  cat "$1" >&2
  exit 1
}

assert() { # $1=sql  $2=expected substring  $3=label
  local out
  if ! out=$($TSQ -c "$1" 2>&1); then
    echo "FAIL [$3]: statement errored: $out" >&2
    exit 1
  fi
  local norm
  norm=$(tr -s ' ' ' ' <<<"$out") # squeeze column padding
  if ! grep -q "$2" <<<"$norm"; then
    echo "FAIL [$3]: output '$out' lacks '$2'" >&2
    exit 1
  fi
}

assert_fail() { # $1=sql  $2=label — must exit non-zero
  if $TSQ -c "$1" >/dev/null 2>&1; then
    echo "FAIL [$2]: expected statement to fail" >&2
    exit 1
  fi
}

start_daemon "$WORK/tsqld1.log"
assert "CREATE TABLE users (id INT PRIMARY KEY AUTO_INCREMENT, name TEXT NOT NULL, score FLOAT)" "OK (0 rows)" "create"
assert "INSERT INTO users (name, score) VALUES ('alfa', 1.5), ('beta', NULL), ('cuki', 9.25)" "OK (3 rows)" "insert"
assert "SELECT name, score FROM users ORDER BY id" "alfa" "select-all"
assert "SELECT name FROM users WHERE score > 5" "cuki" "where"
assert "SELECT name FROM users ORDER BY score DESC LIMIT 1" "cuki" "order-limit"
assert "UPDATE users SET score = 2.0 WHERE name = 'alfa'" "OK (1 row)" "update"
assert "SELECT name, score FROM users WHERE id = 1" "alfa | 2" "update-visible"
assert "DELETE FROM users WHERE name = 'beta'" "OK (1 row)" "delete"
assert_fail "SELECT * FROM nope" "error-path"

# --- Fase 2: JOIN + GROUP BY + UNIQUE --------------------------------------
assert "CREATE TABLE users2 (id INT PRIMARY KEY, name TEXT, dept TEXT)" "OK (0 rows)" "create-users2"
assert "INSERT INTO users2 (id, name, dept) VALUES (1, 'alfa', 'eng'), (2, 'beta', 'sales')" "OK (2 rows)" "seed-users2"
assert "CREATE TABLE orders2 (id INT PRIMARY KEY, user_id INT, amount FLOAT, email TEXT UNIQUE)" "OK (0 rows)" "create-orders2"
assert "INSERT INTO orders2 (id, user_id, amount, email) VALUES (10, 1, 100.0, 'a@x'), (11, 1, 50.5, 'b@x'), (12, 2, 75.0, 'c@x')" "OK (3 rows)" "seed-orders2"
assert "SELECT u2.name, o2.amount FROM users2 u2 JOIN orders2 o2 ON o2.user_id = u2.id ORDER BY o2.amount" "alfa" "join"
assert "SELECT u2.name, o2.amount FROM users2 u2 JOIN orders2 o2 ON o2.user_id = u2.id ORDER BY o2.amount" "beta | 75" "join-row"
assert "SELECT dept, count(*), sum(amount) FROM users2 u2 JOIN orders2 o2 ON o2.user_id = u2.id GROUP BY dept ORDER BY dept" "eng | 2 | 150.5" "group-by"
assert_fail "INSERT INTO orders2 (id, user_id, amount, email) VALUES (13, 2, 10, 'a@x')" "unique-violation"
assert "SELECT id FROM orders2 WHERE email = 'b@x'" "11" "unique-lookup"

# --- Fase 3: transactions + foreign keys ------------------------------------
# Transactions need one connection for BEGIN..COMMIT: pipe lines through the REPL.
tx() { # $1 = newline-separated SQL statements, run on a single connection
  printf '%s\n\\q\n' "$1" | $TSQ 2>&1
}

assert "CREATE TABLE depts (id INT PRIMARY KEY, name TEXT)" "OK (0 rows)" "create-depts"
assert "CREATE TABLE emp (id INT PRIMARY KEY AUTO_INCREMENT, dept_id INT REFERENCES depts)" "OK (0 rows)" "create-emp-fk"
assert "INSERT INTO depts VALUES (1, 'eng'), (2, 'sales')" "OK (2 rows)" "seed-depts"

tx "BEGIN
INSERT INTO emp (dept_id) VALUES (1)
ROLLBACK" >/dev/null
assert "SELECT count(*) FROM emp" "0" "txn-rollback-invisible"

tx "BEGIN
INSERT INTO emp (dept_id) VALUES (1)
COMMIT" >/dev/null
assert "SELECT count(*) FROM emp" "1" "txn-commit-visible"

assert_fail "INSERT INTO emp (dept_id) VALUES (99)" "fk-violation"
assert_fail "DELETE FROM depts WHERE id = 1" "fk-restrict"
assert_fail "COMMIT" "commit-without-txn"

# --- Fase 3: multi-connection (4 parallel clients, no lost writes) -----------
pids=""
for i in 1 2 3 4; do
  $TSQ -c "INSERT INTO depts VALUES ($((100 + i)), 'par$i')" >/dev/null 2>&1 &
  pids="$pids $!"
done
wait $pids
assert "SELECT count(*) FROM depts" "6" "multi-client"

# --- Fase v2: LEFT JOIN + HAVING + IN + subqueries ---------------------------
assert "CREATE TABLE v2u (id INT PRIMARY KEY, name TEXT, dept TEXT)" "OK (0 rows)" "v2-create"
assert "INSERT INTO v2u VALUES (1, 'alfa', 'eng'), (2, 'beta', 'eng'), (3, 'cuki', 'sales')" "OK (3 rows)" "v2-seed"
assert "CREATE TABLE v2o (id INT PRIMARY KEY, uid INT, amt FLOAT)" "OK (0 rows)" "v2-create-o"
assert "INSERT INTO v2o VALUES (10, 1, 100.0), (11, 2, 50.0), (12, 4, 25.0)" "OK (3 rows)" "v2-seed-o"
assert "SELECT u.name, o.amt FROM v2u u LEFT JOIN v2o o ON o.uid = u.id WHERE o.amt IS NULL" "cuki" "left-join-null"
assert "SELECT dept, count(*) FROM v2u GROUP BY dept HAVING count(*) > 1 ORDER BY dept" "eng | 2" "having"
assert "SELECT name FROM v2u WHERE id IN (1, 3) ORDER BY id" "alfa" "in-list"
assert "SELECT name FROM v2u WHERE name NOT IN ('alfa', 'beta')" "cuki" "not-in-list"
assert "SELECT name FROM v2u WHERE id IN (SELECT uid FROM v2o) ORDER BY id" "alfa" "in-subquery"
assert "SELECT name FROM v2u WHERE name IN (SELECT name FROM v2u WHERE dept = 'sales')" "cuki" "in-subquery-text"
assert "SELECT count(*) FROM (SELECT id FROM v2u WHERE id > 0) s" "3" "from-subquery"
assert_fail "SELECT name FROM (SELECT id FROM v2u)" "derived-no-alias"
assert "UPDATE v2u SET dept = 'hr' WHERE id IN (SELECT uid FROM v2o)" "OK (2 rows)" "update-in-subquery"
assert "SELECT dept FROM v2u WHERE id = 3" "sales" "update-in-subquery-check"

# --- crash recovery: kill -9, restart, data must survive ---------------------
kill -9 "$DAEMON_PID" 2>/dev/null || true
wait "$DAEMON_PID" 2>/dev/null || true
DAEMON_PID=""
start_daemon "$WORK/tsqld2.log"
assert "SELECT name FROM users ORDER BY id" "alfa" "crash-recovery"

# --- clean shutdown (SIGTERM -> checkpoint) ----------------------------------
kill -TERM "$DAEMON_PID" 2>/dev/null || true
wait "$DAEMON_PID" 2>/dev/null || true
DAEMON_PID=""
start_daemon "$WORK/tsqld3.log"
assert "SELECT name, score FROM users ORDER BY id" "cuki | 9.25" "clean-shutdown"
# UNIQUE index must survive crash+recovery (rebuilt from data on load)
assert "SELECT id FROM orders2 WHERE email = 'b@x'" "11" "unique-lookup-after-crash"
assert_fail "INSERT INTO orders2 (id, user_id, amount, email) VALUES (14, 1, 5, 'c@x')" "unique-persisted"

echo "E2E PASS (port $PORT)"
