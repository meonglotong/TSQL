package storage

import (
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/meonglotong/tsql/internal/schema"
	"github.com/meonglotong/tsql/internal/types"
)

// ErrTxnDone is returned when a committed or rolled-back txn is used again.
var ErrTxnDone = errors.New("tsql: transaction no longer active")

// Accessor is a read/write view of the tables. *Engine implements it for
// autocommit; *Txn implements it for an explicit transaction (buffered
// writes, read-your-own-writes, no dirty reads).
type Accessor interface {
	Catalog() []schema.TableDef
	AllRows(name string) ([]int64, [][]types.Value, error)
	Get(name string, rowID int64) ([]types.Value, bool)
	UniqueGet(name string, colIdx int, v types.Value) (int64, bool)
	Put(name string, rowID int64, row []types.Value) (int64, error)
	PutAuto(name string, pkIdx int, row []types.Value) (int64, error)
	Update(name string, rowID int64, row []types.Value) error
	Delete(name string, rowID int64) error
}

type opKind int

const (
	opPut opKind = iota
	opDelete
)

type op struct {
	kind  opKind
	name  string
	rowID int64
	row   []types.Value
}

// Txn is one connection's explicit transaction (buffered-write model).
//
// Writes are validated against the committed state plus this txn's own
// buffer, then buffered in memory. Nothing touches the shared tables (or the
// WAL) until Commit, so other connections never see uncommitted data (no
// dirty reads) while the txn sees its own writes (read-your-own-writes).
//
// Commit appends all buffered operations to the WAL with exactly one fsync
// (at the commit record) and then applies them to the shared tables. A crash
// before the commit record is durable loses nothing: recovery replays only
// committed transactions.
type Txn struct {
	e    *Engine
	id   uint64
	done bool

	mu  sync.Mutex
	ops []op
	// inserts tracks rows this txn created (absent from the effective state
	// when buffered). At commit, if one was committed by someone else in the
	// meantime, that is a primary-key conflict (ErrUnique).
	inserts map[string]map[int64]bool
}

// Begin starts a new explicit transaction on the engine.
func (e *Engine) Begin() (*Txn, error) {
	return &Txn{e: e, id: e.txSeq.Add(1), inserts: map[string]map[int64]bool{}}, nil
}

// ID returns the WAL transaction id.
func (t *Txn) ID() uint64 { return t.id }

// Catalog delegates to the engine (schema is not transactional in v1).
func (t *Txn) Catalog() []schema.TableDef { return t.e.Catalog() }

// --- reads (buffer overlays committed state) ---------------------------------

// Get returns the effective row for this txn.
func (t *Txn) Get(name string, rowID int64) ([]types.Value, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return t.e.Get(name, rowID)
	}
	for i := len(t.ops) - 1; i >= 0; i-- {
		o := &t.ops[i]
		if o.name == name && o.rowID == rowID {
			if o.kind == opPut {
				cp := make([]types.Value, len(o.row))
				copy(cp, o.row)
				return cp, true
			}
			return nil, false
		}
	}
	return t.e.Get(name, rowID)
}

// AllRows returns the effective rows for this txn, sorted by row id.
func (t *Txn) AllRows(name string) ([]int64, [][]types.Value, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return t.e.AllRows(name)
	}
	return t.effRowsLocked(name)
}

// effMapLocked merges committed rows with the txn buffer (in op order).
// Caller holds t.mu.
func (t *Txn) effMapLocked(name string) (map[int64][]types.Value, error) {
	cids, grows, err := t.e.AllRows(name)
	if err != nil {
		return nil, err
	}
	m := make(map[int64][]types.Value, len(grows))
	for i, id := range cids {
		m[id] = grows[i]
	}
	for i := range t.ops {
		o := &t.ops[i]
		if o.name != name {
			continue
		}
		if o.kind == opPut {
			m[o.rowID] = o.row
		} else {
			delete(m, o.rowID)
		}
	}
	return m, nil
}

// effRowsLocked merges committed rows with the txn buffer and returns them
// sorted by row id. Caller holds t.mu.
func (t *Txn) effRowsLocked(name string) ([]int64, [][]types.Value, error) {
	m, err := t.effMapLocked(name)
	if err != nil {
		return nil, nil, err
	}
	ids := make([]int64, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	rows := make([][]types.Value, len(ids))
	for i, id := range ids {
		rows[i] = m[id]
	}
	return ids, rows, nil
}

// UniqueGet finds the row id holding value v in a UNIQUE column of the
// effective state. The committed-state index is bypassed because the buffer
// may override it (scan: fine for a dev DB).
func (t *Txn) UniqueGet(name string, colIdx int, v types.Value) (int64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if v.IsNull() {
		return 0, false
	}
	if !t.done {
		ids, rows, err := t.effRowsLocked(name)
		if err != nil {
			return 0, false
		}
		k := uniqKey(v)
		for i, id := range ids {
			if len(rows[i]) > colIdx && uniqKey(rows[i][colIdx]) == k {
				return id, true
			}
		}
		return 0, false
	}
	return t.e.UniqueGet(name, colIdx, v)
}

// --- writes -------------------------------------------------------------------

func (t *Txn) tableDef(name string) (*schema.TableDef, error) {
	for _, d := range t.e.Catalog() {
		if d.Name == name {
			return &d, nil
		}
	}
	return nil, ErrNoTable
}

// checkUnique verifies row against the effective state for every UNIQUE
// column of def, ignoring the row's own id. NULLs are allowed multiple times.
func (t *Txn) checkUnique(def *schema.TableDef, selfID int64, row []types.Value, eff map[int64][]types.Value) error {
	for ci, c := range def.Cols {
		if !c.Unique {
			continue
		}
		v := row[ci]
		if v.IsNull() {
			continue
		}
		k := uniqKey(v)
		for id, r := range eff {
			if id == selfID || len(r) <= ci {
				continue
			}
			if uniqKey(r[ci]) == k {
				return ErrUnique
			}
		}
	}
	return nil
}

// Put inserts a row (rowID 0 assigns the next auto id). The row is buffered;
// it is not visible to other connections until Commit.
func (t *Txn) Put(name string, rowID int64, row []types.Value) (int64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return 0, ErrTxnDone
	}
	def, err := t.tableDef(name)
	if err != nil {
		return 0, err
	}
	if len(row) != len(def.Cols) {
		return 0, fmt.Errorf("tsql: row has %d values, table %s has %d columns", len(row), name, len(def.Cols))
	}
	eff, err := t.effMapLocked(name)
	if err != nil {
		return 0, err
	}
	if rowID == 0 {
		rowID, err = t.e.ReserveID(name)
		if err != nil {
			return 0, err
		}
	}
	if _, exists := eff[rowID]; exists {
		return 0, ErrExists
	}
	if err := t.checkUnique(def, rowID, row, eff); err != nil {
		return 0, err
	}
	t.noteInsert(name, rowID)
	t.ops = append(t.ops, op{kind: opPut, name: name, rowID: rowID, row: row})
	return rowID, nil
}

// PutAuto inserts a row with a fresh auto id and stamps it into the pkIdx
// cell. It mutates row.
func (t *Txn) PutAuto(name string, pkIdx int, row []types.Value) (int64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return 0, ErrTxnDone
	}
	def, err := t.tableDef(name)
	if err != nil {
		return 0, err
	}
	if len(row) != len(def.Cols) {
		return 0, fmt.Errorf("tsql: row has %d values, table %s has %d columns", len(row), name, len(def.Cols))
	}
	eff, err := t.effMapLocked(name)
	if err != nil {
		return 0, err
	}
	rowID, err := t.e.ReserveID(name)
	if err != nil {
		return 0, err
	}
	if _, exists := eff[rowID]; exists {
		return 0, ErrExists
	}
	row[pkIdx] = types.Int(rowID)
	if err := t.checkUnique(def, rowID, row, eff); err != nil {
		return 0, err
	}
	t.noteInsert(name, rowID)
	t.ops = append(t.ops, op{kind: opPut, name: name, rowID: rowID, row: row})
	return rowID, nil
}

// noteInsert records that this txn created row (not pre-existing). Caller
// holds t.mu.
func (t *Txn) noteInsert(name string, rowID int64) {
	if t.inserts[name] == nil {
		t.inserts[name] = map[int64]bool{}
	}
	t.inserts[name][rowID] = true
}

// Update replaces an existing row in place (the row must exist in the
// effective state).
func (t *Txn) Update(name string, rowID int64, row []types.Value) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return ErrTxnDone
	}
	def, err := t.tableDef(name)
	if err != nil {
		return err
	}
	if len(row) != len(def.Cols) {
		return fmt.Errorf("tsql: row has %d values, table %s has %d columns", len(row), name, len(def.Cols))
	}
	eff, err := t.effMapLocked(name)
	if err != nil {
		return err
	}
	if _, exists := eff[rowID]; !exists {
		return ErrNoRow
	}
	if err := t.checkUnique(def, rowID, row, eff); err != nil {
		return err
	}
	t.ops = append(t.ops, op{kind: opPut, name: name, rowID: rowID, row: row})
	return nil
}

// Delete removes one row by id (the row must exist in the effective state).
func (t *Txn) Delete(name string, rowID int64) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return ErrTxnDone
	}
	if _, err := t.tableDef(name); err != nil {
		return err
	}
	eff, err := t.effMapLocked(name)
	if err != nil {
		return err
	}
	if _, exists := eff[rowID]; !exists {
		return ErrNoRow
	}
	t.ops = append(t.ops, op{kind: opDelete, name: name, rowID: rowID})
	return nil
}

// --- lifecycle -----------------------------------------------------------------

// Commit appends the buffered operations to the WAL (one fsync at the commit
// record) and applies them to the shared tables. If a concurrent transaction
// committed a conflicting UNIQUE value in the meantime, Commit fails with
// ErrUnique and the txn stays open (use Rollback).
func (t *Txn) Commit() error {
	t.mu.Lock()
	if t.done {
		t.mu.Unlock()
		return nil
	}
	// Re-validate uniqueness against the freshest committed state plus the
	// buffer (write-time checks may be stale).
	byTable := map[string]*struct {
		def *schema.TableDef
		put []op
	}{}
	for i := range t.ops {
		o := &t.ops[i]
		tc, ok := byTable[o.name]
		if !ok {
			def, err := t.tableDef(o.name)
			if err != nil {
				t.mu.Unlock()
				return err
			}
			tc = &struct {
				def *schema.TableDef
				put []op
			}{def: def}
			byTable[o.name] = tc
		}
		if o.kind == opPut {
			tc.put = append(tc.put, *o)
		}
	}
	for name, tc := range byTable {
		eff, err := t.effMapLocked(name)
		if err != nil {
			t.mu.Unlock()
			return err
		}
		for i := range tc.put {
			if err := t.checkUnique(tc.def, tc.put[i].rowID, tc.put[i].row, eff); err != nil {
				t.mu.Unlock()
				return err
			}
		}
	}
	// PK conflicts: a row this txn created may have been committed by
	// another txn in the meantime (last-committer-wins applies to updates
	// only, not to inserts over a freshly taken primary key).
	for name, m := range t.inserts {
		for rowID := range m {
			if _, ok := t.e.Get(name, rowID); ok {
				t.mu.Unlock()
				return ErrUnique
			}
		}
	}
	ops := t.ops
	id := t.id
	t.ops = nil
	t.done = true
	t.mu.Unlock()

	if len(ops) == 0 {
		return nil
	}
	return t.e.commitTxn(id, ops)
}

// Rollback discards the buffered operations. The WAL is untouched (records
// are only appended at commit time).
func (t *Txn) Rollback() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return nil
	}
	t.ops = nil
	t.done = true
	return nil
}

// commitTxn appends the buffered ops of one txn to the WAL with a single
// fsync at the commit record, then applies them to the shared tables.
// Serialized against Checkpoint (commitMu) so a committed txn is always
// covered by either the WAL or the latest snapshot.
func (e *Engine) commitTxn(id uint64, ops []op) error {
	recs := make([]Record, 0, len(ops)+1)
	for _, o := range ops {
		tid, ok := e.TableID(o.name)
		if !ok {
			return fmt.Errorf("tsql: table %q vanished before commit", o.name)
		}
		if o.kind == opPut {
			recs = append(recs, Record{Type: RecPut, Txn: id, TableID: tid, RowID: o.rowID, Row: o.row})
		} else {
			recs = append(recs, Record{Type: RecDelete, Txn: id, TableID: tid, RowID: o.rowID})
		}
	}
	recs = append(recs, Record{Type: RecCommit, Txn: id})

	e.commitMu.Lock()
	defer e.commitMu.Unlock()
	for i := range recs {
		if _, err := e.wal.Append(recs[i], recs[i].Type == RecCommit); err != nil {
			return err
		}
	}
	for _, o := range ops {
		if o.kind == opPut {
			if err := e.applyPut(o.name, o.rowID, o.row); err != nil {
				return err
			}
		} else {
			if err := e.applyDelete(o.name, o.rowID); err != nil {
				return err
			}
		}
	}
	e.noteWrite()
	return nil
}
