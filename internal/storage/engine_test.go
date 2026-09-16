package storage

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/meonglotong/tsql/internal/schema"
	"github.com/meonglotong/tsql/internal/types"
)

func testCols() []schema.Column {
	return []schema.Column{
		{Name: "id", Type: types.TypeInt, Primary: true, AutoInc: true},
		{Name: "name", Type: types.TypeText, NotNull: true},
		{Name: "score", Type: types.TypeFloat},
	}
}

func mustCreate(t *testing.T, e *Engine) {
	t.Helper()
	if err := e.CreateTable("users", testCols()); err != nil {
		t.Fatalf("create table: %v", err)
	}
}

func mustPut(t *testing.T, e *Engine, id int64, name string, score float64) int64 {
	t.Helper()
	rid, err := e.Put("users", id, []types.Value{types.Int(id), types.Text(name), types.Float(score)})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	return rid
}

func TestCreatePutGetDelete(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer e.Close()
	mustCreate(t, e)

	if err := e.CreateTable("users", testCols()); err != ErrExists {
		t.Errorf("duplicate create: got %v, want ErrExists", err)
	}
	id := mustPut(t, e, 0, "alfa", 1.5)
	if id != 1 {
		t.Errorf("auto id = %d, want 1", id)
	}
	mustPut(t, e, 2, "beta", 2.5)

	row, ok := e.Get("users", 2)
	if !ok || row[1].String() != "beta" {
		t.Errorf("get(2) = %v, %v", row, ok)
	}
	if _, ok := e.Get("users", 99); ok {
		t.Error("get(99) should miss")
	}
	ids, rows, err := e.AllRows("users")
	if err != nil || len(ids) != 2 {
		t.Fatalf("allrows: err=%v n=%d", err, len(ids))
	}
	if ids[0] != 1 || ids[1] != 2 {
		t.Errorf("ids not sorted: %v", ids)
	}
	if len(rows[1]) != 3 || rows[1][2].String() != "2.5" {
		t.Errorf("rows[1] = %v", rows[1])
	}
	if err := e.Delete("users", 1); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := e.Delete("users", 1); err != ErrNoRow {
		t.Errorf("double delete: got %v, want ErrNoRow", err)
	}
	if err := e.DropTable("users"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, _, err := e.AllRows("users"); err != ErrNoTable {
		t.Errorf("after drop: got %v, want ErrNoTable", err)
	}
}

func TestAutoIncAfterExplicitID(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer e.Close()
	mustCreate(t, e)
	mustPut(t, e, 100, "x", 0)
	id := mustPut(t, e, 0, "y", 0)
	if id != 101 {
		t.Errorf("auto id = %d, want 101", id)
	}
}

func TestCreateTableValidation(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer e.Close()
	if err := e.CreateTable("bad name", testCols()); err == nil {
		t.Error("invalid table name accepted")
	}
	if err := e.CreateTable("t", []schema.Column{{Name: "a", Type: types.TypeText}, {Name: "a", Type: types.TypeInt}}); err == nil {
		t.Error("duplicate column accepted")
	}
	if err := e.CreateTable("t", []schema.Column{{Name: "a", Type: types.ColumnType("varchar")}}); err == nil {
		t.Error("unknown type accepted")
	}
	if err := e.CreateTable("t", nil); err == nil {
		t.Error("no columns accepted")
	}
	if err := e.CreateTable("t2", []schema.Column{{Name: "id", Type: types.TypeText, Primary: true}}); err != nil {
		t.Fatalf("create t2: %v", err)
	}
	defs := e.Catalog()
	if len(defs) != 1 || defs[0].Name != "t2" || defs[0].Cols[0].Type != types.TypeInt {
		t.Errorf("pk type not normalized: %+v", defs)
	}
}

func TestReopenAfterClose(t *testing.T) {
	dir := t.TempDir()
	e, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	mustCreate(t, e)
	for i := 1; i <= 25; i++ {
		mustPut(t, e, int64(i), "n", float64(i))
	}
	if err := e.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	e2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer e2.Close()
	ids, rows, err := e2.AllRows("users")
	if err != nil || len(ids) != 25 {
		t.Fatalf("rows = %d, err = %v", len(ids), err)
	}
	if rows[24][2].String() != "25" {
		t.Errorf("last row score = %v", rows[24][2].String())
	}
	id := mustPut(t, e2, 0, "x", 0)
	if id != 26 {
		t.Errorf("auto id after reopen = %d, want 26", id)
	}
}

func TestCrashRecovery(t *testing.T) {
	dir := t.TempDir()
	e, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	mustCreate(t, e)
	for i := 1; i <= 50; i++ {
		mustPut(t, e, 0, "u", float64(i))
	}
	if err := e.Abort(); err != nil { // power loss: no checkpoint
		t.Fatalf("abort: %v", err)
	}
	e2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer e2.Close()
	ids, rows, err := e2.AllRows("users")
	if err != nil || len(ids) != 50 {
		t.Fatalf("after crash recovery: %d rows, err %v", len(ids), err)
	}
	if rows[0][1].String() != "u" {
		t.Errorf("row content lost: %v", rows[0])
	}
}

func TestTornTailTruncated(t *testing.T) {
	dir := t.TempDir()
	e, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	mustCreate(t, e)
	for i := 1; i <= 5; i++ {
		mustPut(t, e, int64(i), "n", 0)
	}
	if err := e.Abort(); err != nil {
		t.Fatalf("abort: %v", err)
	}
	walPath := filepath.Join(dir, "tsql", "wal.log")
	info, _ := os.Stat(walPath)
	validSize := info.Size()
	f, err := os.OpenFile(walPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	f.Write([]byte{0xDE, 0xAD, 0xBE}) // torn record
	f.Close()

	e2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer e2.Close()
	ids, _, err := e2.AllRows("users")
	if err != nil || len(ids) != 5 {
		t.Fatalf("rows after torn tail: %d, err %v", len(ids), err)
	}
	info2, _ := os.Stat(walPath)
	if info2.Size() != validSize {
		t.Errorf("wal not truncated to valid prefix: %d != %d", info2.Size(), validSize)
	}
}

func TestCheckpointThenCrash(t *testing.T) {
	dir := t.TempDir()
	e, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	mustCreate(t, e)
	for i := 1; i <= 10; i++ {
		mustPut(t, e, int64(i), "a", 0)
	}
	if err := e.Checkpoint(); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	for i := 11; i <= 15; i++ {
		mustPut(t, e, int64(i), "b", 0)
	}
	if err := e.Abort(); err != nil { // crash: snapshot(10) + wal(5) must combine
		t.Fatalf("abort: %v", err)
	}
	e2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer e2.Close()
	ids, rows, err := e2.AllRows("users")
	if err != nil || len(ids) != 15 {
		t.Fatalf("rows = %d, err = %v", len(ids), err)
	}
	if rows[14][1].String() != "b" {
		t.Errorf("post-checkpoint rows lost: %v", rows[14])
	}
}

func TestUncommittedTxnDropped(t *testing.T) {
	dir := t.TempDir()
	e, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	mustCreate(t, e)
	mustPut(t, e, 1, "kept", 0)

	// raw uncommitted txn straight into the WAL
	id, ok := e.TableID("users")
	if !ok {
		t.Fatal("no table id")
	}
	e.wal.Append(Record{Type: RecBegin, Txn: 999}, false)
	e.wal.Append(Record{Type: RecPut, Txn: 999, TableID: id, RowID: 500, Row: []types.Value{types.Int(500), types.Text("lost"), types.Float(0)}}, false)
	// no commit record
	if err := e.Abort(); err != nil {
		t.Fatalf("abort: %v", err)
	}
	e2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer e2.Close()
	ids, _, err := e2.AllRows("users")
	if err != nil || len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("uncommitted row survived: ids=%v err=%v", ids, err)
	}
}

func TestConcurrentReadsWrites(t *testing.T) {
	e, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	mustCreate(t, e)

	var wg sync.WaitGroup
	done := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				ids, _, err := e.AllRows("users")
				if err != nil {
					t.Error(err)
					return
				}
				if len(ids) > 2000 {
					t.Error("too many rows observed")
					return
				}
			}
		}()
	}
	for i := 1; i <= 2000; i++ {
		if _, err := e.Put("users", int64(i), []types.Value{types.Int(int64(i)), types.Text("r"), types.Float(0)}); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	close(done)
	wg.Wait()
	ids, _, err := e.AllRows("users")
	if err != nil || len(ids) != 2000 {
		t.Fatalf("final rows = %d, err = %v", len(ids), err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}
