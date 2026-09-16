// Package storage is the TSQL storage engine: in-memory tables backed by a
// write-ahead log (WAL) and periodic snapshots.
//
// Concurrency model (v1, lock-based): writes are serialized through the WAL
// (single writer), reads run concurrently under per-table read locks.
// Durability: exactly one fsync per commit record; a crash loses nothing
// after the last commit (recovery replays the WAL over the latest snapshot).
package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/meonglotong/tsql/internal/schema"
	"github.com/meonglotong/tsql/internal/types"
)

var (
	ErrNoTable = errors.New("no such table")
	ErrExists  = errors.New("object already exists")
	ErrNoRow   = errors.New("no such row")
	ErrUnique  = errors.New("unique constraint violated")
)

// autoCheckpointEvery triggers a snapshot + WAL truncate after this many writes.
const autoCheckpointEvery = 10000

// Engine is the TSQL storage engine.
type Engine struct {
	dir string

	catalogMu   sync.RWMutex
	tables      map[string]*Table
	byID        map[uint32]*Table
	nextTableID uint32

	wal *WAL

	// commitMu serializes explicit-txn commits (WAL append + in-memory apply)
	// against checkpoints (snapshot + WAL truncate), so a committed txn is
	// always covered by either the WAL or the latest snapshot.
	commitMu sync.Mutex

	txSeq           atomic.Uint64
	sinceCheckpoint atomic.Int64
	checkpointing   atomic.Bool
}

// Table is an in-memory table. Rows are stored in Cols order.
// Row id 0 is reserved for "auto-assign" and can never be stored.
type Table struct {
	id   uint32
	name string
	cols []schema.Column

	mu     sync.RWMutex
	rows   map[int64][]types.Value
	nextID int64
	uniq   map[int]map[string]int64 // colIdx -> key -> rowID, UNIQUE columns
}

// ID returns the stable table id used in WAL records.
func (t *Table) ID() uint32 { return t.id }

// Open opens (or creates) the engine under datadir/tsql. Recovery: load the
// latest snapshot, replay committed WAL records, drop uncommitted ones, and
// truncate any torn WAL tail.
func Open(datadir string) (*Engine, error) {
	dir := filepath.Join(datadir, "tsql")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	wal, recs, err := OpenWAL(filepath.Join(dir, "wal.log"))
	if err != nil {
		return nil, fmt.Errorf("tsql: open wal: %w", err)
	}
	e := &Engine{
		dir:    dir,
		wal:    wal,
		tables: map[string]*Table{},
		byID:   map[uint32]*Table{},
	}
	if snap, err := LoadLatestSnapshot(dir); err != nil {
		wal.Close()
		return nil, fmt.Errorf("tsql: load snapshot: %w", err)
	} else if snap != nil {
		for name, st := range snap.Tables {
			t := &Table{id: st.ID, name: st.Name, cols: st.Cols, rows: st.Rows, nextID: st.NextID}
			t.initUniq()
			t.buildUniqLocked()
			e.tables[name] = t
			e.byID[t.id] = t
			if st.ID >= e.nextTableID {
				e.nextTableID = st.ID + 1
			}
		}
	}
	// Replay committed transactions only; uncommitted ones are the rollback.
	pending := map[uint64][]Record{}
	for _, r := range recs {
		switch r.Type {
		case RecBegin:
		case RecCommit:
			if err := e.applyPendingLocked(pending, r.Txn); err != nil {
				wal.Close()
				return nil, fmt.Errorf("tsql: wal replay: %w", err)
			}
		default:
			pending[r.Txn] = append(pending[r.Txn], r)
		}
	}
	return e, nil
}

// CreateTable registers a new table (logged in the WAL).
func (e *Engine) CreateTable(name string, cols []schema.Column) error {
	colsCopy := make([]schema.Column, len(cols))
	copy(colsCopy, cols)
	td := &schema.TableDef{Name: name, Cols: colsCopy}
	if err := td.Validate(); err != nil {
		return err
	}
	e.catalogMu.Lock()
	defer e.catalogMu.Unlock()
	if _, ok := e.tables[name]; ok {
		return ErrExists
	}
	if err := e.txn(func(tx uint64) error {
		_, err := e.wal.Append(Record{Type: RecCreate, Txn: tx, DDL: &SchemaChange{Create: true, Name: name, Cols: td.Cols}}, false)
		return err
	}); err != nil {
		return err
	}
	e.addTableLocked(name, td.Cols)
	e.writeMetaLocked()
	return nil
}

// DropTable removes a table (logged in the WAL).
func (e *Engine) DropTable(name string) error {
	e.catalogMu.Lock()
	defer e.catalogMu.Unlock()
	t, ok := e.tables[name]
	if !ok {
		return ErrNoTable
	}
	if err := e.txn(func(tx uint64) error {
		_, err := e.wal.Append(Record{Type: RecDrop, Txn: tx, DDL: &SchemaChange{Name: name}}, false)
		return err
	}); err != nil {
		return err
	}
	delete(e.tables, t.name)
	delete(e.byID, t.id)
	e.writeMetaLocked()
	return nil
}

// Put inserts a row. rowID 0 assigns the next auto id; the returned id is
// the one actually stored. Inserting an already-existing id is an error (no
// upsert). Values are stored as-is; type checking happens in the executor.
func (e *Engine) Put(name string, rowID int64, row []types.Value) (int64, error) {
	return e.put(name, -1, rowID, row, false)
}

// PutAuto inserts a row with a fresh auto id and stamps the assigned id into
// the pkIdx cell in a single WAL transaction. It mutates row.
func (e *Engine) PutAuto(name string, pkIdx int, row []types.Value) (int64, error) {
	return e.put(name, pkIdx, 0, row, false)
}

// Update replaces an existing row in place. The row id must exist.
func (e *Engine) Update(name string, rowID int64, row []types.Value) error {
	_, err := e.put(name, -1, rowID, row, true)
	return err
}

// put is the shared write path: reserve/validate the id, append the WAL put
// (durable at the commit record), then apply in memory.
func (e *Engine) put(name string, pkIdx int, rowID int64, row []types.Value, mustExist bool) (int64, error) {
	if rowID < 0 {
		return 0, fmt.Errorf("tsql: row id must be >= 0 (0 = auto)")
	}
	if mustExist && rowID == 0 {
		return 0, fmt.Errorf("tsql: update requires an explicit row id")
	}
	e.catalogMu.RLock()
	t := e.tables[name]
	e.catalogMu.RUnlock()
	if t == nil {
		return 0, ErrNoTable
	}
	t.mu.Lock()
	if len(row) != len(t.cols) {
		t.mu.Unlock()
		return 0, fmt.Errorf("tsql: row has %d values, table %s has %d columns", len(row), name, len(t.cols))
	}
	exists := rowID != 0
	if exists {
		_, exists = t.rows[rowID]
	}
	if rowID == 0 {
		rowID = t.nextID
		t.nextID++
		exists = false
	} else if exists && !mustExist {
		t.mu.Unlock()
		return 0, ErrExists
	} else if !exists && mustExist {
		t.mu.Unlock()
		return 0, ErrNoRow
	}
	if !exists && rowID >= t.nextID {
		t.nextID = rowID + 1
	}
	if pkIdx >= 0 && pkIdx < len(row) {
		row[pkIdx] = types.Int(rowID)
	}
	// UNIQUE check — must happen in the same critical section as the apply
	// below (lock is held across the WAL append) so two concurrent writers
	// cannot both pass.
	for _, ci := range t.uniqCols() {
		v := row[ci]
		if v.IsNull() {
			continue // multiple NULLs allowed in a UNIQUE column
		}
		if old, ok := t.uniq[ci][uniqKey(v)]; ok && old != rowID {
			t.mu.Unlock()
			return 0, ErrUnique
		}
	}
	// WAL (durable) — t.mu intentionally held across the append.
	if err := e.txn(func(tx uint64) error {
		_, err := e.wal.Append(Record{Type: RecPut, Txn: tx, TableID: t.id, RowID: rowID, Row: row}, false)
		return err
	}); err != nil {
		t.mu.Unlock()
		return 0, err
	}
	// Apply in memory.
	if oldRow, ok := t.rows[rowID]; ok {
		t.removeRowKeysLocked(rowID, oldRow)
	}
	t.rows[rowID] = row
	t.setRowKeysLocked(rowID, row)
	t.mu.Unlock()
	e.noteWrite()
	return rowID, nil
}

// Delete removes one row by id.
func (e *Engine) Delete(name string, rowID int64) error {
	e.catalogMu.RLock()
	t := e.tables[name]
	e.catalogMu.RUnlock()
	if t == nil {
		return ErrNoTable
	}
	t.mu.Lock()
	old, ok := t.rows[rowID]
	if !ok {
		t.mu.Unlock()
		return ErrNoRow
	}
	if err := e.txn(func(tx uint64) error {
		_, err := e.wal.Append(Record{Type: RecDelete, Txn: tx, TableID: t.id, RowID: rowID}, false)
		return err
	}); err != nil {
		t.mu.Unlock()
		return err
	}
	t.removeRowKeysLocked(rowID, old)
	delete(t.rows, rowID)
	t.mu.Unlock()
	e.noteWrite()
	return nil
}

// Get returns a copy of the row at rowID.
func (e *Engine) Get(name string, rowID int64) ([]types.Value, bool) {
	e.catalogMu.RLock()
	t := e.tables[name]
	e.catalogMu.RUnlock()
	if t == nil {
		return nil, false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	row, ok := t.rows[rowID]
	if !ok {
		return nil, false
	}
	cp := make([]types.Value, len(row))
	copy(cp, row)
	return cp, true
}

// AllRows returns sorted row ids and copied rows.
func (e *Engine) AllRows(name string) ([]int64, [][]types.Value, error) {
	e.catalogMu.RLock()
	t := e.tables[name]
	e.catalogMu.RUnlock()
	if t == nil {
		return nil, nil, ErrNoTable
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	ids := make([]int64, 0, len(t.rows))
	for id := range t.rows {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	rows := make([][]types.Value, len(ids))
	for i, id := range ids {
		r := t.rows[id]
		cp := make([]types.Value, len(r))
		copy(cp, r)
		rows[i] = cp
	}
	return ids, rows, nil
}

// Catalog returns all table definitions, sorted by name.
func (e *Engine) Catalog() []schema.TableDef {
	e.catalogMu.RLock()
	defer e.catalogMu.RUnlock()
	return e.catalogLocked()
}

// TableID returns the stable WAL id of a table.
func (e *Engine) TableID(name string) (uint32, bool) {
	e.catalogMu.RLock()
	defer e.catalogMu.RUnlock()
	t, ok := e.tables[name]
	if !ok {
		return 0, false
	}
	return t.id, true
}

// Checkpoint writes a full snapshot, updates the manifest, truncates the WAL,
// and prunes old snapshots. Briefly blocks writers. Serialized against
// explicit-txn commits via commitMu.
func (e *Engine) Checkpoint() error {
	e.commitMu.Lock()
	defer e.commitMu.Unlock()
	e.catalogMu.Lock()
	defer e.catalogMu.Unlock()
	d := &snapshotData{Seq: e.wal.Seq, Tables: make(map[string]snapshotTable, len(e.tables))}
	for name, t := range e.tables {
		t.mu.Lock()
		rows := make(map[int64][]types.Value, len(t.rows))
		for k, v := range t.rows {
			rows[k] = v
		}
		cols := make([]schema.Column, len(t.cols))
		copy(cols, t.cols)
		d.Tables[name] = snapshotTable{ID: t.id, Name: name, Cols: cols, Rows: rows, NextID: t.nextID}
		t.mu.Unlock()
	}
	name, err := WriteSnapshot(e.dir, d)
	if err != nil {
		return err
	}
	if err := writeManifest(e.dir, name); err != nil {
		return err
	}
	if err := e.wal.Truncate(); err != nil {
		return err
	}
	e.sinceCheckpoint.Store(0)
	e.pruneSnapshots()
	return nil
}

// Close checkpoints (snapshot + WAL truncate) and closes the WAL.
func (e *Engine) Close() error {
	if err := e.Checkpoint(); err != nil {
		return err
	}
	return e.wal.Close()
}

// Abort closes the WAL without a checkpoint. No committed data is lost (every
// commit is fsynced); the next Open recovers from the WAL. Tests use this to
// simulate a power loss.
func (e *Engine) Abort() error {
	return e.wal.Close()
}

// --- internals ---------------------------------------------------------------

// txn wraps fn in a WAL begin/commit pair with exactly one fsync, at the
// commit record. If fn fails, no commit is written: recovery drops the txn.
func (e *Engine) txn(fn func(tx uint64) error) error {
	tx := e.txSeq.Add(1)
	if _, err := e.wal.Append(Record{Type: RecBegin, Txn: tx}, false); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	_, err := e.wal.Append(Record{Type: RecCommit, Txn: tx}, true)
	return err
}

func (e *Engine) addTableLocked(name string, cols []schema.Column) {
	colsCopy := make([]schema.Column, len(cols))
	copy(colsCopy, cols)
	t := &Table{id: e.nextTableID, name: name, cols: colsCopy, rows: map[int64][]types.Value{}, nextID: 1}
	t.initUniq()
	e.nextTableID++
	e.tables[name] = t
	e.byID[t.id] = t
}

// applyPendingLocked applies all buffered records of one committed txn, in
// order. Called during recovery only.
func (e *Engine) applyPendingLocked(pending map[uint64][]Record, tx uint64) error {
	recs := pending[tx]
	delete(pending, tx)
	for _, r := range recs {
		if err := e.applyRecord(r); err != nil {
			return err
		}
	}
	return nil
}

// applyRecord mutates engine state for one WAL record. Recovery only
// (single-threaded, before serving).
func (e *Engine) applyRecord(r Record) error {
	switch r.Type {
	case RecCreate:
		if r.DDL == nil {
			return errors.New("tsql: corrupt create record")
		}
		if _, ok := e.tables[r.DDL.Name]; ok {
			return nil
		}
		e.addTableLocked(r.DDL.Name, r.DDL.Cols)
	case RecDrop:
		if r.DDL == nil {
			return errors.New("tsql: corrupt drop record")
		}
		if t, ok := e.tables[r.DDL.Name]; ok {
			delete(e.tables, t.name)
			delete(e.byID, t.id)
		}
	case RecPut:
		t := e.byID[r.TableID]
		if t == nil {
			return fmt.Errorf("tsql: wal put references unknown table id %d", r.TableID)
		}
		if old, ok := t.rows[r.RowID]; ok {
			t.removeRowKeysLocked(r.RowID, old)
		}
		t.rows[r.RowID] = r.Row
		if r.RowID >= t.nextID {
			t.nextID = r.RowID + 1
		}
		t.setRowKeysLocked(r.RowID, r.Row)
	case RecDelete:
		t := e.byID[r.TableID]
		if t == nil {
			return fmt.Errorf("tsql: wal delete references unknown table id %d", r.TableID)
		}
		if old, ok := t.rows[r.RowID]; ok {
			t.removeRowKeysLocked(r.RowID, old)
			delete(t.rows, r.RowID)
		}
	}
	return nil
}

// --- UNIQUE secondary index ------------------------------------------------

// uniqCols returns the indices of columns declared UNIQUE.
func (t *Table) uniqCols() []int {
	var out []int
	for i, c := range t.cols {
		if c.Unique {
			out = append(out, i)
		}
	}
	return out
}

// uniqKey builds a collision-free key for a value, prefixed with its kind so
// that e.g. int 1 and text "1" never collide.
func uniqKey(v types.Value) string {
	switch v.Kind {
	case types.KindInt:
		return "i" + strconv.FormatInt(v.I, 10)
	case types.KindFloat:
		return "f" + strconv.FormatFloat(v.F, 'g', -1, 64)
	case types.KindText:
		return "s" + v.S
	case types.KindBool:
		if v.B {
			return "b1"
		}
		return "b0"
	case types.KindTime:
		return "t" + v.T.UTC().Format(time.RFC3339Nano)
	default:
		return "n"
	}
}

// initUniq allocates the per-column unique-index maps.
func (t *Table) initUniq() {
	t.uniq = make(map[int]map[string]int64)
	for _, ci := range t.uniqCols() {
		t.uniq[ci] = make(map[string]int64)
	}
}

// buildUniqLocked rebuilds the unique index from all current rows. Caller
// holds t.mu (or the table is not yet shared during recovery).
func (t *Table) buildUniqLocked() {
	t.initUniq()
	for id, row := range t.rows {
		t.setRowKeysLocked(id, row)
	}
}

// setRowKeysLocked indexes the unique-column values of row.
func (t *Table) setRowKeysLocked(rowID int64, row []types.Value) {
	for _, ci := range t.uniqCols() {
		v := row[ci]
		if v.IsNull() {
			continue
		}
		t.uniq[ci][uniqKey(v)] = rowID
	}
}

// removeRowKeysLocked un-indexes the unique-column values stored for rowID.
func (t *Table) removeRowKeysLocked(rowID int64, row []types.Value) {
	for _, ci := range t.uniqCols() {
		v := row[ci]
		if v.IsNull() {
			continue
		}
		k := uniqKey(v)
		if cur, ok := t.uniq[ci][k]; ok && cur == rowID {
			delete(t.uniq[ci], k)
		}
	}
}

// UniqueGet looks up the row id holding value v in a UNIQUE column via the
// secondary index. Returns false if not found or the column is not UNIQUE.
func (e *Engine) UniqueGet(name string, colIdx int, v types.Value) (int64, bool) {
	e.catalogMu.RLock()
	t := e.tables[name]
	e.catalogMu.RUnlock()
	if t == nil || v.IsNull() {
		return 0, false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	m, ok := t.uniq[colIdx]
	if !ok {
		return 0, false
	}
	id, ok := m[uniqKey(v)]
	return id, ok
}

// ReserveID allocates the next row id without storing anything. Used by
// explicit transactions; rolled-back reservations leave gaps (normal for
// auto-increment).
func (e *Engine) ReserveID(name string) (int64, error) {
	e.catalogMu.RLock()
	t := e.tables[name]
	e.catalogMu.RUnlock()
	if t == nil {
		return 0, ErrNoTable
	}
	t.mu.Lock()
	id := t.nextID
	t.nextID++
	t.mu.Unlock()
	return id, nil
}

// applyPut applies one row to the in-memory table (no WAL). Used by explicit
// transactions at commit time.
func (e *Engine) applyPut(name string, rowID int64, row []types.Value) error {
	e.catalogMu.RLock()
	t := e.tables[name]
	e.catalogMu.RUnlock()
	if t == nil {
		return ErrNoTable
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if old, ok := t.rows[rowID]; ok {
		t.removeRowKeysLocked(rowID, old)
	}
	t.rows[rowID] = row
	if rowID >= t.nextID {
		t.nextID = rowID + 1
	}
	t.setRowKeysLocked(rowID, row)
	return nil
}

// applyDelete removes one row from the in-memory table (no WAL).
func (e *Engine) applyDelete(name string, rowID int64) error {
	e.catalogMu.RLock()
	t := e.tables[name]
	e.catalogMu.RUnlock()
	if t == nil {
		return ErrNoTable
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if old, ok := t.rows[rowID]; ok {
		t.removeRowKeysLocked(rowID, old)
		delete(t.rows, rowID)
	}
	return nil
}

// pruneSnapshots keeps only the two most recent snapshots.
func (e *Engine) pruneSnapshots() {
	entries, err := os.ReadDir(e.dir)
	if err != nil {
		return
	}
	type sn struct {
		name string
		seq  uint64
	}
	var list []sn
	for _, en := range entries {
		n := en.Name()
		if strings.HasPrefix(n, "snapshot-") && strings.HasSuffix(n, ".bin") {
			var seq uint64
			if _, err := fmt.Sscanf(n, "snapshot-%d.bin", &seq); err == nil {
				list = append(list, sn{n, seq})
			}
		}
	}
	if len(list) <= 2 {
		return
	}
	sort.Slice(list, func(i, j int) bool { return list[i].seq > list[j].seq })
	for _, s := range list[2:] {
		os.Remove(filepath.Join(e.dir, s.name))
	}
}

// noteWrite fires a background checkpoint once the write count since the last
// checkpoint passes the threshold.
func (e *Engine) noteWrite() {
	if e.sinceCheckpoint.Add(1) >= autoCheckpointEvery {
		e.sinceCheckpoint.Store(0)
		if e.checkpointing.CompareAndSwap(false, true) {
			go func() {
				defer e.checkpointing.Store(false)
				_ = e.Checkpoint()
			}()
		}
	}
}

// catalogLocked dumps the catalog. Caller holds catalogMu (read or write).
func (e *Engine) catalogLocked() []schema.TableDef {
	defs := make([]schema.TableDef, 0, len(e.tables))
	for _, t := range e.tables {
		cols := make([]schema.Column, len(t.cols))
		copy(cols, t.cols)
		defs = append(defs, schema.TableDef{Name: t.name, Cols: cols})
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	return defs
}

// writeMetaLocked dumps the catalog to meta.json (convenience copy; the
// source of truth is snapshot + WAL). Best effort. Caller holds catalogMu.
func (e *Engine) writeMetaLocked() {
	b, err := json.MarshalIndent(e.catalogLocked(), "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(filepath.Join(e.dir, "meta.json"), b, 0o644)
}
