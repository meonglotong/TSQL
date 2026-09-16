package executor

import (
	"errors"
	"testing"

	"github.com/meonglotong/tsql/internal/parser"
	"github.com/meonglotong/tsql/internal/storage"
)

const usersSchema = "CREATE TABLE users (id INT PRIMARY KEY AUTO_INCREMENT, name TEXT NOT NULL, score FLOAT)"

func newEng(t *testing.T) *storage.Engine {
	t.Helper()
	e, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

func run(t *testing.T, e *storage.Engine, sql string) *Result {
	t.Helper()
	st, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	res, err := Exec(e, e, st)
	if err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
	return res
}

func mustErr(t *testing.T, e *storage.Engine, sql, wantCode string) {
	t.Helper()
	st, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	_, err = Exec(e, e, st)
	if err == nil {
		t.Fatalf("exec %q: expected error, got none", sql)
	}
	if wantCode != "" {
		sq, ok := err.(*SQLError)
		if !ok {
			t.Fatalf("exec %q: expected SQLError, got %T: %v", sql, err, err)
		}
		if sq.Code != wantCode {
			t.Errorf("exec %q: code %s, want %s", sql, sq.Code, wantCode)
		}
	}
}

// mustErrAcc is mustErr for a non-autocommit accessor (a txn).
func mustErrAcc(t *testing.T, e *storage.Engine, acc storage.Accessor, sql, wantCode string) {
	t.Helper()
	st, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	_, err = Exec(e, acc, st)
	if err == nil {
		t.Fatalf("exec %q: expected error, got none", sql)
	}
	if wantCode != "" {
		sq, ok := err.(*SQLError)
		if !ok {
			t.Fatalf("exec %q: expected SQLError, got %T: %v", sql, err, err)
		}
		if sq.Code != wantCode {
			t.Errorf("exec %q: code %s, want %s", sql, sq.Code, wantCode)
		}
	}
}

// --- Fase 3: foreign keys (RESTRICT) ----------------------------------------

func TestForeignKeys(t *testing.T) {
	e := newEng(t)
	run(t, e, "CREATE TABLE depts (id INT PRIMARY KEY, name TEXT)")
	mustErr(t, e, "CREATE TABLE emp (id INT PRIMARY KEY, dept_id INT REFERENCES nosuch)", "42P01")
	run(t, e, "CREATE TABLE emp (id INT PRIMARY KEY, name TEXT, dept_id INT REFERENCES depts)")
	run(t, e, "INSERT INTO depts VALUES (1, 'eng'), (2, 'sales')")

	// insert with missing parent row is rejected
	mustErr(t, e, "INSERT INTO emp (id, name, dept_id) VALUES (1, 'alfa', 99)", "23503")
	run(t, e, "INSERT INTO emp (id, name, dept_id) VALUES (1, 'alfa', 1)")
	run(t, e, "INSERT INTO emp (id, name) VALUES (2, 'beta')") // NULL FK allowed
	// update to a missing parent row is rejected
	mustErr(t, e, "UPDATE emp SET dept_id = 42 WHERE id = 1", "23503")
	run(t, e, "UPDATE emp SET dept_id = 2 WHERE id = 1")
	// RESTRICT: delete of a referenced parent pk is rejected
	mustErr(t, e, "DELETE FROM depts WHERE id = 2", "23503")
	run(t, e, "DELETE FROM depts WHERE id = 1") // alfa already moved away
	// moving an unreferenced parent pk is fine; moving a referenced one is not
	run(t, e, "INSERT INTO depts VALUES (3, 'hr')")
	run(t, e, "UPDATE depts SET id = 4 WHERE id = 3")
	mustErr(t, e, "UPDATE depts SET id = 9 WHERE id = 2", "23503")
	// create-time checks
	mustErr(t, e, "CREATE TABLE bad (id INT PRIMARY KEY, d TEXT REFERENCES depts)", "42804")
	mustErr(t, e, "CREATE TABLE bad2 (id INT PRIMARY KEY, d INT REFERENCES depts(nope))", "23552")
}

func TestForeignKeysInTxn(t *testing.T) {
	e := newEng(t)
	run(t, e, "CREATE TABLE depts (id INT PRIMARY KEY, name TEXT)")
	run(t, e, "CREATE TABLE emp (id INT PRIMARY KEY AUTO_INCREMENT, dept_id INT REFERENCES depts)")
	run(t, e, "INSERT INTO depts VALUES (1, 'eng')")

	a, err := e.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	// parent row exists only in this txn's buffer — the child insert must pass
	execAcc(t, e, a, "INSERT INTO depts (id, name) VALUES (7, 'x')")
	execAcc(t, e, a, "INSERT INTO emp (dept_id) VALUES (7)")
	// RESTRICT against the merged view: the buffered child blocks the delete
	mustErrAcc(t, e, a, "DELETE FROM depts WHERE id = 7", "23503")
	if err := a.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	res := run(t, e, "SELECT count(*) FROM emp")
	if res.Rows[0][0].I != 1 {
		t.Errorf("emp count after commit = %v", res.Rows[0][0])
	}
}

func seed(t *testing.T, e *storage.Engine) {
	t.Helper()
	run(t, e, usersSchema)
	run(t, e, "INSERT INTO users (name, score) VALUES ('alfa', 1.5), ('beta', NULL), ('cuki', 9.25)")
}

func TestCRUD(t *testing.T) {
	e := newEng(t)
	seed(t, e)

	res := run(t, e, "SELECT * FROM users ORDER BY id")
	if len(res.Rows) != 3 {
		t.Fatalf("rows = %d", len(res.Rows))
	}
	if res.Columns[0] != "id" || res.Columns[1] != "name" || res.Columns[2] != "score" {
		t.Errorf("cols = %v", res.Columns)
	}
	if res.Rows[0][0].I != 1 || res.Rows[0][1].S != "alfa" || res.Rows[0][2].F != 1.5 {
		t.Errorf("row0 = %v", res.Rows[0])
	}
	if !res.Rows[1][2].IsNull() {
		t.Errorf("row1 score should be NULL: %v", res.Rows[1][2])
	}

	res = run(t, e, "SELECT name FROM users WHERE score > 2 ORDER BY score")
	if len(res.Rows) != 1 || res.Rows[0][0].S != "cuki" {
		t.Errorf("where result = %v", res.Rows)
	}

	res = run(t, e, "UPDATE users SET score = 2.0 WHERE name = 'alfa'")
	if res.Affected != 1 {
		t.Errorf("update affected = %d", res.Affected)
	}
	res = run(t, e, "SELECT score FROM users WHERE id = 1")
	if res.Rows[0][0].F != 2.0 {
		t.Errorf("score after update = %v", res.Rows[0][0])
	}

	res = run(t, e, "DELETE FROM users WHERE name = 'beta'")
	if res.Affected != 1 {
		t.Errorf("delete affected = %d", res.Affected)
	}
	res = run(t, e, "SELECT * FROM users")
	if len(res.Rows) != 2 {
		t.Errorf("rows after delete = %d", len(res.Rows))
	}
}

func TestAutoIncrementStamped(t *testing.T) {
	e := newEng(t)
	run(t, e, usersSchema)
	run(t, e, "INSERT INTO users (name, score) VALUES ('alfa', 1)")
	res := run(t, e, "SELECT id, name FROM users")
	if res.Rows[0][0].I != 1 {
		t.Errorf("pk not stamped: id = %v", res.Rows[0][0])
	}
}

func TestLimitOrderBy(t *testing.T) {
	e := newEng(t)
	seed(t, e)
	res := run(t, e, "SELECT name FROM users ORDER BY score DESC LIMIT 1")
	if len(res.Rows) != 1 || res.Rows[0][0].S != "cuki" {
		t.Errorf("got %v", res.Rows)
	}
	res = run(t, e, "SELECT name FROM users ORDER BY id LIMIT 2")
	if len(res.Rows) != 2 {
		t.Errorf("limit 2 gave %d rows", len(res.Rows))
	}
}

func TestErrors(t *testing.T) {
	e := newEng(t)
	mustErr(t, e, "SELECT * FROM nope", "42P01")
	run(t, e, usersSchema)
	mustErr(t, e, "CREATE TABLE users (id INT)", "42P07")
	mustErr(t, e, "SELECT * FROM users WHERE bogus = 1", "42703")
	mustErr(t, e, "INSERT INTO users (score) VALUES (1)", "23502")                   // name NOT NULL
	mustErr(t, e, "INSERT INTO users (name, id) VALUES ('x', 5), ('y', 5)", "23505") // dup pk
	mustErr(t, e, "INSERT INTO users (name, score) VALUES ('x', 'not-a-number')", "22P02")
	run(t, e, "DROP TABLE users")
	mustErr(t, e, "DROP TABLE users", "42P01")
	run(t, e, "DROP TABLE IF EXISTS users") // no error
}

func TestArithmeticAndLogic(t *testing.T) {
	e := newEng(t)
	seed(t, e)

	res := run(t, e, "SELECT score + 1 FROM users WHERE id = 1")
	if res.Rows[0][0].F != 2.5 {
		t.Errorf("score+1 = %v", res.Rows[0][0])
	}
	res = run(t, e, "SELECT name FROM users WHERE score > 1 AND score < 10")
	if len(res.Rows) != 2 {
		t.Errorf("and result = %v", res.Rows)
	}
	// three-valued logic: comparing against NULL matches nothing
	res = run(t, e, "SELECT name FROM users WHERE score = NULL")
	if len(res.Rows) != 0 {
		t.Errorf("null comparison should match nothing: %v", res.Rows)
	}
	// NOT on a null comparison stays null (excluded)
	res = run(t, e, "SELECT name FROM users WHERE NOT (score = NULL)")
	if len(res.Rows) != 0 {
		t.Errorf("NOT(null cmp) should match nothing: %v", res.Rows)
	}
}

// --- Fase 3: transactions (buffered-write model) ------------------------------

// execAcc runs sql against acc (a connection view: engine or txn).
func execAcc(t *testing.T, e *storage.Engine, acc storage.Accessor, sql string) *Result {
	t.Helper()
	st, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	res, err := Exec(e, acc, st)
	if err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
	return res
}

func TestTxnCommitRollback(t *testing.T) {
	e := newEng(t)
	seed(t, e)

	a, err := e.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	execAcc(t, e, a, "INSERT INTO users (name, score) VALUES ('x', 99)")
	// no dirty reads: other connections see committed state only
	res := run(t, e, "SELECT count(*) FROM users")
	if res.Rows[0][0].I != 3 {
		t.Errorf("dirty read: count = %v", res.Rows[0][0])
	}
	// read-your-own-writes
	res = execAcc(t, e, a, "SELECT count(*) FROM users")
	if res.Rows[0][0].I != 4 {
		t.Errorf("read-your-own-writes: count = %v", res.Rows[0][0])
	}
	execAcc(t, e, a, "UPDATE users SET score = 1 WHERE name = 'x'")
	res = execAcc(t, e, a, "SELECT score FROM users WHERE name = 'x'")
	if res.Rows[0][0].F != 1 {
		t.Errorf("update in txn: %v", res.Rows[0][0])
	}
	if err := a.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	res = run(t, e, "SELECT count(*) FROM users")
	if res.Rows[0][0].I != 4 {
		t.Errorf("after commit: count = %v", res.Rows[0][0])
	}
}

func TestTxnRollback(t *testing.T) {
	e := newEng(t)
	seed(t, e)
	a, err := e.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	execAcc(t, e, a, "INSERT INTO users (name, score) VALUES ('x', 1)")
	execAcc(t, e, a, "UPDATE users SET score = 0 WHERE name = 'alfa'")
	execAcc(t, e, a, "DELETE FROM users WHERE name = 'cuki'")
	if err := a.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	res := run(t, e, "SELECT name, score FROM users ORDER BY id")
	if len(res.Rows) != 3 || res.Rows[0][0].S != "alfa" || res.Rows[0][1].F != 1.5 || res.Rows[2][0].S != "cuki" {
		t.Errorf("after rollback: %v", res.Rows)
	}
}

func TestTxnIsolationBetweenTxns(t *testing.T) {
	e := newEng(t)
	seed(t, e)
	a, _ := e.Begin()
	b, _ := e.Begin()
	execAcc(t, e, a, "INSERT INTO users (name, score) VALUES ('x', 1)")
	res := execAcc(t, e, b, "SELECT count(*) FROM users")
	if res.Rows[0][0].I != 3 {
		t.Errorf("b sees a's uncommitted row: count = %v", res.Rows[0][0])
	}
	if err := a.Commit(); err != nil {
		t.Fatalf("a commit: %v", err)
	}
	if err := b.Commit(); err != nil {
		t.Fatalf("b commit (empty): %v", err)
	}
	res = run(t, e, "SELECT count(*) FROM users")
	if res.Rows[0][0].I != 4 {
		t.Errorf("final count = %v", res.Rows[0][0])
	}
}

func TestTxnUniqueConflictAtCommit(t *testing.T) {
	e := newEng(t)
	run(t, e, "CREATE TABLE mail (id INT PRIMARY KEY AUTO_INCREMENT, addr TEXT UNIQUE)")
	a, _ := e.Begin()
	b, _ := e.Begin()
	execAcc(t, e, a, "INSERT INTO mail (addr) VALUES ('x@x')")
	// passes the write-time check (a's row is not committed yet)...
	execAcc(t, e, b, "INSERT INTO mail (addr) VALUES ('x@x')")
	if err := a.Commit(); err != nil {
		t.Fatalf("a commit: %v", err)
	}
	// ...but b's commit must be rejected
	if err := b.Commit(); !errors.Is(err, storage.ErrUnique) {
		t.Errorf("b commit: got %v, want ErrUnique", err)
	}
	// b stays open and can be rolled back; only a's row survives
	_ = b.Rollback()
	res := run(t, e, "SELECT addr FROM mail ORDER BY id")
	if len(res.Rows) != 1 || res.Rows[0][0].S != "x@x" {
		t.Errorf("mail = %v", res.Rows)
	}
}

func TestTimestampCoerce(t *testing.T) {
	e := newEng(t)
	run(t, e, "CREATE TABLE ev (id INT PRIMARY KEY AUTO_INCREMENT, at TIMESTAMP, note TEXT)")
	run(t, e, "INSERT INTO ev (at, note) VALUES ('2026-09-14 03:52:00', 'deploy')")
	res := run(t, e, "SELECT at, note FROM ev")
	if res.Rows[0][0].Kind.String() != "timestamp" {
		t.Errorf("at = %v", res.Rows[0][0])
	}
	if res.Rows[0][1].S != "deploy" {
		t.Errorf("note = %v", res.Rows[0][1])
	}
}

// --- Fase 2: JOIN / GROUP BY / UNIQUE ----------------------------------------

func seedJoin(t *testing.T, e *storage.Engine) {
	t.Helper()
	run(t, e, "CREATE TABLE users (id INT PRIMARY KEY, name TEXT, dept TEXT)")
	run(t, e, "INSERT INTO users VALUES (1, 'alfa', 'eng'), (2, 'beta', 'sales'), (3, 'cuki', 'eng'), (4, 'delta', NULL)")
	run(t, e, "CREATE TABLE orders (id INT PRIMARY KEY, user_id INT, amount FLOAT)")
	run(t, e, "INSERT INTO orders VALUES (10, 1, 100.0), (11, 1, 50.5), (12, 2, 300), (13, 3, 75)")
}

func TestInnerJoin(t *testing.T) {
	e := newEng(t)
	seedJoin(t, e)

	res := run(t, e, "SELECT u.name, o.amount FROM users u JOIN orders o ON o.user_id = u.id ORDER BY o.amount")
	if len(res.Rows) != 4 {
		t.Fatalf("rows = %d: %v", len(res.Rows), res.Rows)
	}
	if res.Columns[0] != "u.name" || res.Columns[1] != "o.amount" {
		t.Errorf("headers = %v", res.Columns)
	}
	want := []struct {
		name   string
		amount float64
	}{{"alfa", 50.5}, {"cuki", 75}, {"alfa", 100}, {"beta", 300}}
	for i, w := range want {
		if res.Rows[i][0].S != w.name || res.Rows[i][1].F != w.amount {
			t.Errorf("row%d = %v, want %s/%.1f", i, res.Rows[i], w.name, w.amount)
		}
	}

	// star: all columns, alias-prefixed headers
	res = run(t, e, "SELECT * FROM users u JOIN orders o ON o.user_id = u.id")
	if len(res.Columns) != 6 || res.Columns[0] != "u.id" || res.Columns[3] != "o.id" {
		t.Errorf("star headers = %v", res.Columns)
	}
	if len(res.Rows) != 4 {
		t.Errorf("star rows = %d", len(res.Rows))
	}

	// count over a join
	res = run(t, e, "SELECT count(*) FROM users u JOIN orders o ON o.user_id = u.id")
	if res.Rows[0][0].I != 4 {
		t.Errorf("count = %v", res.Rows[0][0])
	}

	// WHERE on both sides, ordered
	res = run(t, e, "SELECT u.name FROM users u JOIN orders o ON o.user_id = u.id WHERE o.amount > 80 ORDER BY u.name")
	if len(res.Rows) != 2 || res.Rows[0][0].S != "alfa" || res.Rows[1][0].S != "beta" {
		t.Errorf("joined where = %v", res.Rows)
	}

	// no match -> empty
	res = run(t, e, "SELECT u.name FROM users u JOIN orders o ON o.user_id = u.id WHERE o.amount > 1000")
	if len(res.Rows) != 0 {
		t.Errorf("expected no rows: %v", res.Rows)
	}
}

func TestJoinAmbiguity(t *testing.T) {
	e := newEng(t)
	seedJoin(t, e)
	// both tables have "id": unqualified is ambiguous
	mustErr(t, e, "SELECT id FROM users u JOIN orders o ON o.user_id = u.id", "42703")
	// qualified resolves it
	res := run(t, e, "SELECT u.id FROM users u JOIN orders o ON o.user_id = u.id ORDER BY u.id")
	if len(res.Rows) != 4 || res.Rows[0][0].I != 1 {
		t.Errorf("qualified id = %v", res.Rows)
	}
	// unknown alias
	mustErr(t, e, "SELECT x.id FROM users u JOIN orders o ON o.user_id = u.id", "42P01")
	// duplicate alias
	mustErr(t, e, "SELECT u.id FROM users u JOIN orders u ON u.user_id = u.id", "42703")
}

func TestJoinChain(t *testing.T) {
	e := newEng(t)
	seedJoin(t, e)
	run(t, e, "CREATE TABLE order_items (id INT PRIMARY KEY, order_id INT, item TEXT)")
	run(t, e, "INSERT INTO order_items VALUES (100, 10, 'mouse'), (101, 10, 'cable'), (102, 12, 'keyboard')")
	res := run(t, e, "SELECT u.name, oi.item FROM users u JOIN orders o ON o.user_id = u.id JOIN order_items oi ON oi.order_id = o.id ORDER BY oi.item")
	if len(res.Rows) != 3 {
		t.Fatalf("rows = %d: %v", len(res.Rows), res.Rows)
	}
	if res.Rows[0][1].S != "cable" || res.Rows[1][1].S != "keyboard" || res.Rows[2][1].S != "mouse" {
		t.Errorf("items = %v", res.Rows)
	}
	if res.Rows[0][0].S != "alfa" || res.Rows[1][0].S != "beta" {
		t.Errorf("names = %v", res.Rows)
	}
}

func TestGroupBy(t *testing.T) {
	e := newEng(t)
	seedJoin(t, e)

	res := run(t, e, "SELECT dept, count(*), sum(amount), avg(amount), min(amount), max(amount) FROM users u JOIN orders o ON o.user_id = u.id GROUP BY dept ORDER BY dept")
	if len(res.Rows) != 2 {
		t.Fatalf("groups = %d: %v", len(res.Rows), res.Rows)
	}
	// dept order: "eng" < "sales" (NULL group sorts first by our NULLs-first Compare)
	if res.Rows[0][0].S != "eng" {
		t.Errorf("group0 = %v", res.Rows[0])
	}
	if res.Rows[0][1].I != 3 || res.Rows[0][2].F != 225.5 || res.Rows[0][3].F != 225.5/3 || res.Rows[0][4].F != 50.5 || res.Rows[0][5].F != 100 {
		t.Errorf("eng = %v", res.Rows[0])
	}
	if res.Rows[1][0].S != "sales" || res.Rows[1][1].I != 1 || res.Rows[1][2].F != 300 {
		t.Errorf("sales = %v", res.Rows[1])
	}
	if res.Columns[2] != "sum(amount)" || res.Columns[0] != "u.dept" {
		t.Errorf("headers = %v", res.Columns)
	}

	// single implicit group (aggregates, no GROUP BY)
	res = run(t, e, "SELECT count(*), sum(amount) FROM orders")
	if len(res.Rows) != 1 || res.Rows[0][0].I != 4 || res.Rows[0][1].F != 525.5 {
		t.Errorf("single group = %v", res.Rows)
	}

	// single implicit group over empty input: 1 row, count 0, sum NULL
	res = run(t, e, "SELECT count(*), sum(amount) FROM orders WHERE user_id = 99")
	if len(res.Rows) != 1 || res.Rows[0][0].I != 0 || !res.Rows[0][1].IsNull() {
		t.Errorf("empty group = %v", res.Rows)
	}

	// GROUP BY + WHERE
	res = run(t, e, "SELECT dept, count(*) FROM users u JOIN orders o ON o.user_id = u.id WHERE o.amount >= 100 GROUP BY dept ORDER BY dept")
	if len(res.Rows) != 2 || res.Rows[0][1].I != 1 || res.Rows[1][1].I != 1 {
		t.Errorf("group where = %v", res.Rows)
	}

	// ORDER BY aggregate
	res = run(t, e, "SELECT dept, count(*) FROM users u JOIN orders o ON o.user_id = u.id GROUP BY dept ORDER BY count(*) DESC")
	if res.Rows[0][0].S != "eng" || res.Rows[1][0].S != "sales" {
		t.Errorf("order by agg = %v", res.Rows)
	}

	// GROUP BY multiple columns + LIMIT
	res = run(t, e, "SELECT dept, name, count(*) FROM users u JOIN orders o ON o.user_id = u.id GROUP BY dept, name ORDER BY count(*) DESC, name LIMIT 2")
	if len(res.Rows) != 2 || res.Rows[0][1].S != "alfa" || res.Rows[1][1].S != "beta" {
		t.Errorf("multi group = %v", res.Rows)
	}

	// NULL group key: delta has dept NULL and ends up in its own group;
	// NULLs sort first by our Compare.
	res = run(t, e, "SELECT dept, count(*) FROM users GROUP BY dept ORDER BY dept")
	if len(res.Rows) != 3 {
		t.Fatalf("null group = %v", res.Rows)
	}
	if !res.Rows[0][0].IsNull() || res.Rows[0][1].I != 1 {
		t.Errorf("null group row = %v", res.Rows[0])
	}
	if res.Rows[1][0].S != "eng" || res.Rows[1][1].I != 2 {
		t.Errorf("eng group row = %v", res.Rows[1])
	}
	if res.Rows[2][0].S != "sales" || res.Rows[2][1].I != 1 {
		t.Errorf("sales group row = %v", res.Rows[2])
	}
}

func TestGroupByErrors(t *testing.T) {
	e := newEng(t)
	seedJoin(t, e)
	mustErr(t, e, "SELECT name, count(*) FROM users u JOIN orders o ON o.user_id = u.id GROUP BY dept", "42803")
	mustErr(t, e, "SELECT * FROM users GROUP BY dept", "42803")
	mustErr(t, e, "SELECT u.dept FROM users u JOIN orders o ON o.user_id = u.id GROUP BY u.dept ORDER BY o.amount", "42803")
	mustErr(t, e, "SELECT * FROM users WHERE count(*) > 1", "42883")
	mustErr(t, e, "SELECT sum(*) FROM users", "42883")
	mustErr(t, e, "SELECT count(*) FROM users GROUP BY sum(amount)", "42601")
}

func TestUniqueIndex(t *testing.T) {
	e := newEng(t)
	run(t, e, "CREATE TABLE t (id INT PRIMARY KEY, email TEXT UNIQUE, note TEXT)")
	run(t, e, "INSERT INTO t VALUES (1, 'a@x.com', 'r1')")
	mustErr(t, e, "INSERT INTO t VALUES (2, 'a@x.com', 'r2')", "23505") // dup unique
	run(t, e, "INSERT INTO t VALUES (3, NULL, 'r3')")
	run(t, e, "INSERT INTO t VALUES (4, NULL, 'r4')") // multiple NULLs allowed

	// equality lookup via the index
	res := run(t, e, "SELECT id FROM t WHERE email = 'a@x.com'")
	if len(res.Rows) != 1 || res.Rows[0][0].I != 1 {
		t.Errorf("unique select = %v", res.Rows)
	}
	res = run(t, e, "SELECT id FROM t WHERE email = 'zzz.com'")
	if len(res.Rows) != 0 {
		t.Errorf("unique miss = %v", res.Rows)
	}
	res = run(t, e, "SELECT id FROM t WHERE email = NULL")
	if len(res.Rows) != 0 {
		t.Errorf("null lookup should match nothing: %v", res.Rows)
	}
	// wrong-type literal: no match, no error
	res = run(t, e, "SELECT id FROM t WHERE email = 5")
	if len(res.Rows) != 0 {
		t.Errorf("type mismatch = %v", res.Rows)
	}

	// UPDATE through the index
	mustErr(t, e, "UPDATE t SET email = 'a@x.com' WHERE id = 3", "23505") // collides with row 1
	run(t, e, "UPDATE t SET email = 'b@x.com' WHERE id = 3")
	res = run(t, e, "SELECT id FROM t WHERE email = 'b@x.com'")
	if len(res.Rows) != 1 || res.Rows[0][0].I != 3 {
		t.Errorf("after update = %v", res.Rows)
	}
	run(t, e, "DELETE FROM t WHERE email = 'a@x.com'")
	res = run(t, e, "SELECT id FROM t WHERE email = 'a@x.com'")
	if len(res.Rows) != 0 {
		t.Errorf("after delete = %v", res.Rows)
	}
	// freed value can be reused
	run(t, e, "UPDATE t SET email = 'a@x.com' WHERE id = 4")
	res = run(t, e, "SELECT id FROM t WHERE email = 'a@x.com'")
	if len(res.Rows) != 1 || res.Rows[0][0].I != 4 {
		t.Errorf("reused value = %v", res.Rows)
	}
}

func TestUniqueFloatCoerce(t *testing.T) {
	e := newEng(t)
	run(t, e, "CREATE TABLE f (id INT PRIMARY KEY, n FLOAT UNIQUE)")
	run(t, e, "INSERT INTO f VALUES (1, 5.0)")
	// int literal against a float unique column still hits the index
	res := run(t, e, "SELECT id FROM f WHERE n = 5")
	if len(res.Rows) != 1 || res.Rows[0][0].I != 1 {
		t.Errorf("coerced lookup = %v", res.Rows)
	}
	mustErr(t, e, "INSERT INTO f VALUES (2, 5)", "23505") // 5 == 5.0
}

// --- v2: LEFT JOIN, HAVING, subqueries, IN -----------------------------------

func TestLeftJoin(t *testing.T) {
	e := newEng(t)
	run(t, e, "CREATE TABLE users (id INT PRIMARY KEY, name TEXT)")
	run(t, e, "CREATE TABLE orders (id INT PRIMARY KEY, user_id INT, amount FLOAT)")
	run(t, e, "INSERT INTO users (id, name) VALUES (1, 'alfa'), (2, 'beta')")
	run(t, e, "INSERT INTO orders (id, user_id, amount) VALUES (10, 1, 100.0), (11, 1, 50.0), (12, 3, 75.0)")

	// beta has no orders; user 3's order has no user
	r := run(t, e, "SELECT u.name, o.amount FROM users u LEFT JOIN orders o ON o.user_id = u.id ORDER BY u.id, o.amount")
	if len(r.Rows) != 3 {
		t.Fatalf("rows = %d, want 3: %+v", len(r.Rows), r.Rows)
	}
	if r.Rows[0][0].S != "alfa" || r.Rows[0][1].F != 50 {
		t.Errorf("row0 = %+v", r.Rows[0])
	}
	if r.Rows[2][0].S != "beta" || !r.Rows[2][1].IsNull() {
		t.Errorf("row2 (unmatched left row) = %+v", r.Rows[2])
	}

	// aggregates over the left join: beta counts 0 orders
	r2 := run(t, e, "SELECT u.name, count(o.id) FROM users u LEFT JOIN orders o ON o.user_id = u.id GROUP BY u.name ORDER BY u.name")
	if len(r2.Rows) != 2 || r2.Rows[0][0].S != "alfa" || r2.Rows[0][1].I != 2 ||
		r2.Rows[1][0].S != "beta" || r2.Rows[1][1].I != 0 {
		t.Fatalf("agg rows = %+v", r2.Rows)
	}
}

func TestHaving(t *testing.T) {
	e := newEng(t)
	run(t, e, "CREATE TABLE t (id INT PRIMARY KEY, dept TEXT, v INT)")
	run(t, e, "INSERT INTO t VALUES (1, 'a', 10), (2, 'a', 20), (3, 'b', 5), (4, 'c', 7), (5, 'c', 9)")

	// HAVING count(*) > 1 keeps a and c, drops b
	r := run(t, e, "SELECT dept, count(*) FROM t GROUP BY dept HAVING count(*) > 1 ORDER BY dept")
	if len(r.Rows) != 2 || r.Rows[0][0].S != "a" || r.Rows[0][1].I != 2 || r.Rows[1][0].S != "c" || r.Rows[1][1].I != 2 {
		t.Fatalf("rows = %+v", r.Rows)
	}

	// HAVING on an aggregate of a grouped column
	r2 := run(t, e, "SELECT dept, sum(v) FROM t GROUP BY dept HAVING sum(v) > 15 ORDER BY dept")
	if len(r2.Rows) != 2 || r2.Rows[0][0].S != "a" || r2.Rows[0][1].I != 30 || r2.Rows[1][0].S != "c" || r2.Rows[1][1].I != 16 {
		t.Fatalf("rows = %+v", r2.Rows)
	}

	// HAVING without GROUP BY: one implicit group
	r3 := run(t, e, "SELECT count(*) FROM t HAVING count(*) > 3")
	if len(r3.Rows) != 1 || r3.Rows[0][0].I != 5 {
		t.Fatalf("rows = %+v", r3.Rows)
	}

	// a non-grouped column in HAVING is rejected
	mustErr(t, e, "SELECT dept, v FROM t GROUP BY dept HAVING v > 1", "42803")
	// HAVING on an empty table: no rows survive
	run(t, e, "CREATE TABLE empty (id INT PRIMARY KEY)")
	r4 := run(t, e, "SELECT count(*) FROM empty HAVING count(*) > 0")
	if len(r4.Rows) != 0 {
		t.Fatalf("rows = %+v", r4.Rows)
	}
}

func TestFromSubquery(t *testing.T) {
	e := newEng(t)
	run(t, e, "CREATE TABLE users (id INT PRIMARY KEY, name TEXT, dept TEXT)")
	run(t, e, "INSERT INTO users VALUES (1, 'alfa', 'eng'), (2, 'beta', 'eng'), (3, 'cuki', 'sales')")

	// derived table filters rows
	r := run(t, e, "SELECT name FROM (SELECT id, name FROM users WHERE dept = 'eng') s ORDER BY id")
	if len(r.Rows) != 2 || r.Rows[0][0].S != "alfa" || r.Rows[1][0].S != "beta" {
		t.Fatalf("rows = %+v", r.Rows)
	}

	// derived table feeds an aggregate
	r2 := run(t, e, "SELECT count(*) FROM (SELECT dept FROM users) s")
	if len(r2.Rows) != 1 || r2.Rows[0][0].I != 3 {
		t.Fatalf("rows = %+v", r2.Rows)
	}

	// derived table can be joined
	run(t, e, "CREATE TABLE depts (id INT PRIMARY KEY, code TEXT)")
	run(t, e, "INSERT INTO depts VALUES (1, 'eng'), (3, 'sales')")
	r3 := run(t, e, "SELECT s.name, d.code FROM (SELECT id, name FROM users) s JOIN depts d ON d.id = s.id ORDER BY s.id")
	if len(r3.Rows) != 2 || r3.Rows[0][0].S != "alfa" || r3.Rows[0][1].S != "eng" || r3.Rows[1][0].S != "cuki" || r3.Rows[1][1].S != "sales" {
		t.Fatalf("rows = %+v", r3.Rows)
	}

	// nested derived tables
	r4 := run(t, e, "SELECT count(*) FROM (SELECT count(*) FROM (SELECT id FROM users WHERE id > 0) i) o")
	if len(r4.Rows) != 1 || r4.Rows[0][0].I != 1 {
		t.Fatalf("rows = %+v", r4.Rows)
	}
}

func TestIn(t *testing.T) {
	e := newEng(t)
	run(t, e, "CREATE TABLE t (id INT PRIMARY KEY, name TEXT)")
	run(t, e, "INSERT INTO t VALUES (1, 'a'), (2, 'b'), (3, 'c'), (4, 'd')")

	// literal list
	r := run(t, e, "SELECT name FROM t WHERE id IN (3, 1) ORDER BY id")
	if len(r.Rows) != 2 || r.Rows[0][0].S != "a" || r.Rows[1][0].S != "c" {
		t.Fatalf("rows = %+v", r.Rows)
	}
	r2 := run(t, e, "SELECT name FROM t WHERE id NOT IN (1, 3) ORDER BY id")
	if len(r2.Rows) != 2 || r2.Rows[0][0].S != "b" || r2.Rows[1][0].S != "d" {
		t.Fatalf("rows = %+v", r2.Rows)
	}

	// IN subquery
	run(t, e, "CREATE TABLE keep (id INT PRIMARY KEY)")
	run(t, e, "INSERT INTO keep VALUES (1), (3)")
	r3 := run(t, e, "SELECT name FROM t WHERE id IN (SELECT id FROM keep) ORDER BY id")
	if len(r3.Rows) != 2 || r3.Rows[0][0].S != "a" || r3.Rows[1][0].S != "c" {
		t.Fatalf("rows = %+v", r3.Rows)
	}

	// NOT IN subquery; empty subquery result
	r4 := run(t, e, "SELECT name FROM t WHERE id NOT IN (SELECT id FROM keep) ORDER BY id")
	if len(r4.Rows) != 2 || r4.Rows[0][0].S != "b" || r4.Rows[1][0].S != "d" {
		t.Fatalf("rows = %+v", r4.Rows)
	}
	r5 := run(t, e, "SELECT name FROM t WHERE id IN (SELECT id FROM keep WHERE id = 99) ORDER BY id")
	if len(r5.Rows) != 0 {
		t.Fatalf("rows = %+v", r5.Rows)
	}
	r6 := run(t, e, "SELECT name FROM t WHERE id NOT IN (SELECT id FROM keep WHERE id = 99) ORDER BY id")
	if len(r6.Rows) != 4 {
		t.Fatalf("rows = %+v", r6.Rows)
	}

	// IN inside a WHERE on an UPDATE
	run(t, e, "UPDATE t SET name = 'X' WHERE id IN (SELECT id FROM keep)")
	r7 := run(t, e, "SELECT count(*) FROM t WHERE name = 'X'")
	if r7.Rows[0][0].I != 2 {
		t.Fatalf("rows = %+v", r7.Rows)
	}
}
