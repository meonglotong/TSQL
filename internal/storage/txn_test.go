package storage

import (
	"testing"

	"github.com/meonglotong/tsql/internal/schema"
	"github.com/meonglotong/tsql/internal/types"
)

func uniqueCols() []schema.Column {
	return []schema.Column{
		{Name: "id", Type: types.TypeInt, Primary: true, AutoInc: true},
		{Name: "email", Type: types.TypeText, Unique: true},
	}
}

func mustBegin(t *testing.T, e *Engine) *Txn {
	t.Helper()
	txn, err := e.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	return txn
}

func TestTxnCommitVisibility(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer e.Close()
	mustCreate(t, e)

	txn := mustBegin(t, e)
	id, err := txn.PutAuto("users", 0, []types.Value{types.Int(0), types.Text("hidden"), types.Float(1)})
	if err != nil {
		t.Fatalf("txn put: %v", err)
	}
	// no dirty reads: the committed view (engine) must not see it yet
	if _, ok := e.Get("users", id); ok {
		t.Error("dirty read: uncommitted row visible via engine")
	}
	ids, _, _ := e.AllRows("users")
	if len(ids) != 0 {
		t.Errorf("dirty read via AllRows: %v", ids)
	}
	if err := txn.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	row, ok := e.Get("users", id)
	if !ok || row[1].String() != "hidden" || row[0].I != id {
		t.Errorf("committed row = %v %v", row, ok)
	}
}

func TestTxnRollback(t *testing.T) {
	dir := t.TempDir()
	e, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	mustCreate(t, e)
	txn := mustBegin(t, e)
	if _, err := txn.PutAuto("users", 0, []types.Value{types.Int(0), types.Text("gone"), types.Float(0)}); err != nil {
		t.Fatalf("txn put: %v", err)
	}
	if err := txn.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if ids, _, _ := e.AllRows("users"); len(ids) != 0 {
		t.Errorf("rolled-back row survived: %v", ids)
	}
	// crash with an in-flight txn: nothing committed must survive
	txn2 := mustBegin(t, e)
	if _, err := txn2.PutAuto("users", 0, []types.Value{types.Int(0), types.Text("crash"), types.Float(0)}); err != nil {
		t.Fatalf("txn2 put: %v", err)
	}
	if err := e.Abort(); err != nil { // power loss mid-transaction
		t.Fatalf("abort: %v", err)
	}
	e2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer e2.Close()
	if ids, _, err := e2.AllRows("users"); err != nil || len(ids) != 0 {
		t.Errorf("after crash: %d rows, err %v", len(ids), err)
	}
}

func TestTxnReadYourOwnWrites(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer e.Close()
	mustCreate(t, e)

	txn := mustBegin(t, e)
	id, err := txn.Put("users", 10, []types.Value{types.Int(10), types.Text("a"), types.Float(0)})
	if err != nil || id != 10 {
		t.Fatalf("put id 10: id=%d err=%v", id, err)
	}
	// read-your-own-writes
	row, ok := txn.Get("users", 10)
	if !ok || row[1].String() != "a" {
		t.Fatalf("own read = %v %v", row, ok)
	}
	// another connection (engine view) must not see it
	if _, ok := e.Get("users", 10); ok {
		t.Error("dirty read via engine")
	}
	// update within the txn
	if err := txn.Update("users", 10, []types.Value{types.Int(10), types.Text("b"), types.Float(2)}); err != nil {
		t.Fatalf("txn update: %v", err)
	}
	row, _ = txn.Get("users", 10)
	if row[1].String() != "b" {
		t.Errorf("own read after update = %v", row)
	}
	// delete within the txn
	if err := txn.Delete("users", 10); err != nil {
		t.Fatalf("txn delete: %v", err)
	}
	if _, ok := txn.Get("users", 10); ok {
		t.Error("own read after delete should miss")
	}
	ids, rows, err := txn.AllRows("users")
	if err != nil || len(ids) != 0 {
		t.Fatalf("own allrows = %v %v err=%v", ids, rows, err)
	}
	if err := txn.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, ok := e.Get("users", 10); ok {
		t.Error("row should be gone after commit")
	}
}

func TestTxnWriteTimeChecks(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer e.Close()
	mustCreate(t, e)
	mustPut(t, e, 5, "committed", 0)

	txn := mustBegin(t, e)
	// insert an id that exists in committed state -> ErrExists at write time
	if _, err := txn.Put("users", 5, []types.Value{types.Int(5), types.Text("x"), types.Float(0)}); err != ErrExists {
		t.Errorf("dup insert: got %v, want ErrExists", err)
	}
	// update a missing row -> ErrNoRow
	if err := txn.Update("users", 999, []types.Value{types.Int(999), types.Text("x"), types.Float(0)}); err != ErrNoRow {
		t.Errorf("missing update: got %v, want ErrNoRow", err)
	}
	// update a row inserted by the same txn (buffered) -> ok
	id, err := txn.Put("users", 7, []types.Value{types.Int(7), types.Text("n"), types.Float(0)})
	if err != nil {
		t.Fatalf("buffered put: %v", err)
	}
	if err := txn.Update("users", id, []types.Value{types.Int(7), types.Text("m"), types.Float(0)}); err != nil {
		t.Errorf("update own buffered row: %v", err)
	}
	// delete a row only present in this txn's buffer -> ok
	if err := txn.Delete("users", id); err != nil {
		t.Errorf("delete own buffered row: %v", err)
	}
	if err := txn.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	ids, _, _ := e.AllRows("users")
	if len(ids) != 1 || ids[0] != 5 {
		t.Errorf("final rows = %v", ids)
	}
}

func TestTxnUniqueConflictOnCommit(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer e.Close()
	if err := e.CreateTable("mail", uniqueCols()); err != nil {
		t.Fatalf("create: %v", err)
	}
	a := mustBegin(t, e)
	b := mustBegin(t, e)
	if _, err := a.PutAuto("mail", 0, []types.Value{types.Int(0), types.Text("x@x")}); err != nil {
		t.Fatalf("a put: %v", err)
	}
	// same unique value in a second txn: passes the write-time check (not yet
	// committed anywhere) but must fail at commit time
	if _, err := b.PutAuto("mail", 0, []types.Value{types.Int(0), types.Text("x@x")}); err != nil {
		t.Fatalf("b put: %v", err)
	}
	if err := a.Commit(); err != nil {
		t.Fatalf("a commit: %v", err)
	}
	if err := b.Commit(); err != ErrUnique {
		t.Errorf("b commit: got %v, want ErrUnique", err)
	}
	ids, rows, _ := e.AllRows("mail")
	if len(ids) != 1 || rows[0][1].String() != "x@x" {
		t.Errorf("final mail = %v %v", ids, rows)
	}
	// multiple NULLs in a unique column stay allowed (fresh txn: the first
	// one is already done after its commit)
	a2 := mustBegin(t, e)
	if _, err := a2.PutAuto("mail", 0, []types.Value{types.Int(0), types.Text("y@x")}); err != nil {
		t.Fatalf("a2 put: %v", err)
	}
	c := mustBegin(t, e)
	if _, err := c.PutAuto("mail", 0, []types.Value{types.Int(0), types.Null()}); err != nil {
		t.Fatalf("c null: %v", err)
	}
	if err := a2.Commit(); err != nil {
		t.Fatalf("a2 commit: %v", err)
	}
	if err := c.Commit(); err != nil {
		t.Fatalf("c commit: %v", err)
	}
}

func TestTxnAutoIncGapAfterRollback(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer e.Close()
	mustCreate(t, e)
	txn := mustBegin(t, e)
	id1, err := txn.PutAuto("users", 0, []types.Value{types.Int(0), types.Text("gap"), types.Float(0)})
	if err != nil || id1 != 1 {
		t.Fatalf("putauto: id=%d err=%v", id1, err)
	}
	if err := txn.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	id2, err := e.PutAuto("users", 0, []types.Value{types.Int(0), types.Text("ok"), types.Float(0)})
	if err != nil || id2 != 2 {
		t.Fatalf("autocommit putauto: id=%d err=%v (gap ok, collision not)", id2, err)
	}
}

func TestTxnConcurrentIsolation(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer e.Close()
	mustCreate(t, e)

	// two txns: the second must not see the first's uncommitted rows
	a := mustBegin(t, e)
	b := mustBegin(t, e)
	if _, err := a.PutAuto("users", 0, []types.Value{types.Int(0), types.Text("a"), types.Float(0)}); err != nil {
		t.Fatalf("a put: %v", err)
	}
	ids, _, err := b.AllRows("users")
	if err != nil || len(ids) != 0 {
		t.Fatalf("b sees a's uncommitted rows: %v err=%v", ids, err)
	}
	// interleaved commits of disjoint rows both survive
	if _, err := a.PutAuto("users", 0, []types.Value{types.Int(0), types.Text("a2"), types.Float(0)}); err != nil {
		t.Fatalf("a put2: %v", err)
	}
	if _, err := b.PutAuto("users", 0, []types.Value{types.Int(0), types.Text("b"), types.Float(0)}); err != nil {
		t.Fatalf("b put: %v", err)
	}
	if err := b.Commit(); err != nil {
		t.Fatalf("b commit: %v", err)
	}
	if err := a.Commit(); err != nil {
		t.Fatalf("a commit: %v", err)
	}
	ids, rows, err := e.AllRows("users")
	if err != nil || len(ids) != 3 {
		t.Fatalf("final = %v err=%v", ids, err)
	}
	_ = rows
}
